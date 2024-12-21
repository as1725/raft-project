package raftkv

import (
	"encoding/gob"
	"labrpc"
	"log"
	"raft"
	"sync"
	"time"
)

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

type Op struct {
	Operation string
	Key       string
	Value     string
	ClientID  int64
	RequestID int64
}

type RaftKV struct {
	mu           sync.Mutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	maxraftstate int // snapshot if log grows this big

	kvStore       map[string]string
	lastRequestID map[int64]int64
	resultCh      map[int]chan Op
}

func (kv *RaftKV) isLeadCheck(op Op, reply *GetReply) (int, bool) {
    index, _, isLeader := kv.rf.Start(op)
    if !isLeader {
        reply.WrongLeader = true
        return 0, false
    }
    return index, true
}

func (kv *RaftKV) Get(args *GetArgs, reply *GetReply) {
	op := Op{
		Operation: "Get",
		Key:       args.Key,
		ClientID:  args.ClientID,
		RequestID: args.RequestID,
	}

	index, isLeader := kv.isLeadCheck(op, reply)
	if !isLeader {
		return
	}

	kv.mu.Lock()
	ch := make(chan Op, 1)
	kv.resultCh[index] = ch
	kv.mu.Unlock()

	select {
	case committedOp := <-ch:
		if committedOp.ClientID == op.ClientID && committedOp.RequestID == op.RequestID {
			kv.mu.Lock()
			value, exists := kv.kvStore[op.Key]
			kv.mu.Unlock()
			if exists {
				reply.Value = value
				reply.Err = OK
			} else {
				reply.Err = ErrNoKey
			}
		} else {
			reply.WrongLeader = true
		}
	case <-time.After(500 * time.Millisecond):
		reply.WrongLeader = true
	}

	kv.mu.Lock()
	delete(kv.resultCh, index)
	kv.mu.Unlock()
}

func (kv *RaftKV) isLeadCheck2(op Op, reply *PutAppendReply) (int, bool) {
	index, _, isLeader := kv.rf.Start(op)
	if !isLeader {
		reply.WrongLeader = true
		return 0, false
	}
	return index, true
}

func (kv *RaftKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	operation := Op{
		RequestID: args.RequestID,
		ClientID:  args.ClientID,
		Key:       args.Key,
		Value:     args.Value,
		Operation: args.Op,
	}

	index, isLeader := kv.isLeadCheck2(operation, reply)
	if !isLeader {
		return
	}

	kv.mu.Lock()
	ch := make(chan Op, 1)
	kv.resultCh[index] = ch
	kv.mu.Unlock()

	select {
	case committedOp := <-ch:
		if committedOp.ClientID == operation.ClientID && committedOp.RequestID == operation.RequestID {
			reply.Err = OK
		} else {
			reply.WrongLeader = true
		}
	case <-time.After(500 * time.Millisecond):
		reply.WrongLeader = true
	}

	kv.mu.Lock()
	delete(kv.resultCh, index)
	kv.mu.Unlock()
}

func (kv *RaftKV) applyOperations() {
	for msg := range kv.applyCh {
		op := msg.Command.(Op)

		kv.mu.Lock()
		lastReqID, exists := kv.lastRequestID[op.ClientID]
		
		if  (op.RequestID > lastReqID || !exists) && op.Operation == "Put" {
			kv.kvStore[op.Key] = op.Value
			kv.lastRequestID[op.ClientID] = op.RequestID
		} else if (op.RequestID > lastReqID || !exists) && op.Operation == "Append" {
			kv.kvStore[op.Key] += op.Value
			kv.lastRequestID[op.ClientID] = op.RequestID
		}

		if ch, ok := kv.resultCh[msg.Index]; ok {
			ch <- op
		}
		kv.mu.Unlock()
	}
}

func (kv *RaftKV) Kill() {
	kv.rf.Kill()
	// Your code here, if desired.
}

func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *RaftKV {
	// call gob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	gob.Register(Op{})

	kv := new(RaftKV)
	kv.me = me
	kv.maxraftstate = maxraftstate

	kv.kvStore = make(map[string]string)
	kv.lastRequestID = make(map[int64]int64)
	kv.resultCh = make(map[int]chan Op)

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	go kv.applyOperations()

	return kv
}
