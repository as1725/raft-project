package raftkv

import (
	"labrpc"
	crand "crypto/rand"
	"math/big"
	"math/rand"
	"time"
)

type Clerk struct {
	servers   []*labrpc.ClientEnd
	leaderID  int
	clientID  int64
	requestID int64
}

func nrand() int64 {
	max := big.NewInt(int64(1) << 62)
	bigx, _ := crand.Int(crand.Reader, max)
	x := bigx.Int64()
	return x
}

func MakeClerk(servers []*labrpc.ClientEnd) *Clerk {
	ck := new(Clerk)
	ck.servers = servers
	ck.leaderID = rand.Intn(len(servers))
	ck.clientID = nrand()
	ck.requestID = 0
	return ck
}

func (ck *Clerk) performRequest(method string, args interface{}) interface{} {
	for {
		var reply interface{}
		if method == "RaftKV.Get" {
			reply = &GetReply{}
		} else if method == "RaftKV.PutAppend" {
			reply = &PutAppendReply{}
		}
		ok := ck.servers[ck.leaderID].Call(method, args, reply)
		if ok {
			switch r := reply.(type) {
			case *GetReply:
				if !r.WrongLeader && r.Err == OK {
					return r.Value
				}
			case *PutAppendReply:
				if !r.WrongLeader && r.Err == OK {
					return nil
				}
			}
		}
		ck.leaderID = (ck.leaderID + 1) % len(ck.servers)
		time.Sleep(50 * time.Millisecond)
	}
}

func (ck *Clerk) Get(key string) string {
	ck.requestID++
	args := GetArgs{
		RequestID: ck.requestID,
		ClientID:  ck.clientID, 
		Key:       key,
	}
	result := ck.performRequest("RaftKV.Get", &args)
	return result.(string)
}

func (ck *Clerk) PutAppend(key string, value string, op string) {
	ck.requestID++
	args := PutAppendArgs{
		RequestID: ck.requestID,
		ClientID:  ck.clientID,
		Op:        op,
		Value:     value,
		Key:       key,
	}
	ck.performRequest("RaftKV.PutAppend", &args)
}

func (ck *Clerk) Put(key string, value string) {
	ck.PutAppend(key, value, "Put")
}

func (ck *Clerk) Append(key string, value string) {
	ck.PutAppend(key, value, "Append")
}
