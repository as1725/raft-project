package raft

import (
	"bytes"
	"encoding/gob"
	"labrpc"
	"math/rand"
	"sync"
	"time"
)

type ApplyMsg struct {
	Index       int
	Command     interface{}
	UseSnapshot bool
	Snapshot    []byte
}

type Raft struct {
	mu        sync.Mutex
	peers     []*labrpc.ClientEnd
	persister *Persister
	me        int

	currentTerm int
	votedFor    int

	electionTimeout        time.Duration
	resetElectionTimeoutCh chan struct{}
	isLeader               bool
	isKilled               bool

	log         []LogEntry
	commitIndex int
	lastApplied int
	nextIndex   []int
	matchIndex  []int
	applyCh     chan ApplyMsg
}

type LogEntry struct {
	Term    int
	Command interface{}
}

type RequestVoteArgs struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term          int
	Success       bool
	ConflictTerm  int
	ConflictIndex int
}

func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.currentTerm, rf.isLeader
}

func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := gob.NewEncoder(w)
	e.Encode(rf.currentTerm)
	e.Encode(rf.votedFor)
	e.Encode(rf.log)
	data := w.Bytes()
	rf.persister.SaveRaftState(data)
}

func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) == 0 {
		return
	}
	r := bytes.NewBuffer(data)
	d := gob.NewDecoder(r)
	d.Decode(&rf.currentTerm)
	d.Decode(&rf.votedFor)
	d.Decode(&rf.log)
}

func (rf *Raft) startElection() {
	rf.currentTerm++
	rf.votedFor = rf.me
	rf.electionTimeoutReset()
	rf.persist()

	DPrintf("Node %d starting election for term %d", rf.me, rf.currentTerm)
	lastLogTerm := 0
	if len(rf.log) > 0 {
		lastLogTerm = rf.log[len(rf.log)-1].Term
	}
	args := RequestVoteArgs{
		Term:         rf.currentTerm,
		CandidateId:  rf.me,
		LastLogIndex: len(rf.log) - 1,
		LastLogTerm:  lastLogTerm,
	}
	var votes int32 = 1
	for i := range rf.peers {
		if i != rf.me {
			go func(i int) {
				rf.mu.Lock()
				rf.mu.Unlock()
				var reply RequestVoteReply
				if rf.sendRequestVote(i, args, &reply) {
					rf.mu.Lock()
					defer rf.mu.Unlock()

					if reply.Term > rf.currentTerm {
						rf.stepDown(reply.Term)
						return
					}

					if reply.VoteGranted {
						votes++
						DPrintf("Node %d received vote from %d for term %d", rf.me, i, rf.currentTerm)
						if votes > int32(len(rf.peers)/2) && !rf.isLeader {
							DPrintf("Node %d became leader for term %d", rf.me, rf.currentTerm)
							rf.isLeader = true
							for i := range rf.peers {
								rf.nextIndex[i] = len(rf.log)
								rf.matchIndex[i] = 0
							}
							go rf.sendHeartbeats()
						}
					}
				}
			}(i)
		}
	}
}

func (rf *Raft) RequestVote(args RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	DPrintf("Node %d received RequestVote from %d for term %d (current term: %d)", rf.me, args.CandidateId, args.Term, rf.currentTerm)

	if args.Term > rf.currentTerm {
		rf.stepDown(args.Term)
	}

	if args.Term < rf.currentTerm {
		DPrintf("Node %d received RequestVote from %d for term %d < current term %d, replying false", rf.me, args.CandidateId, args.Term, rf.currentTerm)
		reply.Term = rf.currentTerm
		reply.VoteGranted = false
		return
	}

	if args.Term == rf.currentTerm && rf.isValidLog(args) {
		rf.votedFor = args.CandidateId
		reply.VoteGranted = true
		rf.electionTimeoutReset()
	} else {
		reply.VoteGranted = false
	}

	reply.Term = rf.currentTerm
	rf.persist()
	DPrintf("Node %d voted for %d for term %d: %t", rf.me, args.CandidateId, args.Term, reply.VoteGranted)
}

func (rf *Raft) isValidLog(args RequestVoteArgs) bool {
	if rf.votedFor == -1 || rf.votedFor == args.CandidateId {
		if len(rf.log) == 0 || args.LastLogIndex == -1 {
			return true
		}
		if args.LastLogTerm > rf.log[len(rf.log)-1].Term {
			return true
		}

		if args.LastLogTerm == rf.log[len(rf.log)-1].Term && args.LastLogIndex >= len(rf.log)-1 {
			return true
		}
	}
	return false
}

func (rf *Raft) sendHeartbeats() {
	for !rf.killed() {
		rf.mu.Lock()
		if rf.isLeader {
			for peer := range rf.peers {
				if peer != rf.me {
					go func(peer int) {
						rf.mu.Lock()
						if !rf.isLeader {
							rf.mu.Unlock()
							return
						}
						prevLogIndex := rf.nextIndex[peer] - 1
						prevLogTerm := 0
						if prevLogIndex >= 0 {
							prevLogTerm = rf.log[prevLogIndex].Term
						}

						args := AppendEntriesArgs{
							Term:         rf.currentTerm,
							LeaderId:     rf.me,
							PrevLogIndex: prevLogIndex,
							PrevLogTerm:  prevLogTerm,
							Entries:      rf.log[rf.nextIndex[peer]:],
							LeaderCommit: rf.commitIndex,
						}
						rf.mu.Unlock()

						var reply AppendEntriesReply
						if rf.sendAppendEntries(peer, args, &reply) {
							rf.mu.Lock()
							defer rf.mu.Unlock()

							if reply.Term > rf.currentTerm {
								rf.stepDown(reply.Term)
								return
							}

							if reply.Success {
								rf.matchIndex[peer] = args.PrevLogIndex + len(args.Entries)
								rf.nextIndex[peer] = rf.matchIndex[peer] + 1
								for i := len(rf.log) - 1; i > rf.commitIndex; i-- {
									count := 1
									for j := range rf.peers {
										if j != rf.me && rf.matchIndex[j] >= i {
											count++
										}
									}
									if count > len(rf.peers)/2 {
										rf.commitIndex = i
										rf.applyLogEntries()
										break
									}
								}
							} else {
								rf.nextIndex[peer] = reply.ConflictIndex
							}
						}
					}(peer)
				}
			}
		}
		rf.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
}

func (rf *Raft) AppendEntries(args AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	DPrintf("Node %d received AppendEntries from %d for term %d (current term: %d)", rf.me, args.LeaderId, args.Term, rf.currentTerm)

	if args.Term < rf.currentTerm {
		reply.Term = rf.currentTerm
		reply.Success = false
		reply.ConflictIndex = -1
		reply.ConflictTerm = -1
		return
	}

	if args.Term > rf.currentTerm {
		rf.stepDown(args.Term)
	}

	rf.electionTimeoutReset()

	if args.PrevLogIndex > -1 {
		if args.PrevLogIndex >= len(rf.log) {
			reply.Success = false
			reply.Term = rf.currentTerm
			reply.ConflictIndex = len(rf.log)
			reply.ConflictTerm = -1
			return
		}

		if rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
			reply.Success = false
			reply.Term = rf.currentTerm
			reply.ConflictTerm = rf.log[args.PrevLogIndex].Term
			tempInd := args.PrevLogIndex
			for tempInd > 0 && rf.log[tempInd-1].Term == reply.ConflictTerm {
				tempInd--
			}
			reply.ConflictIndex = tempInd
			return
		}
	}

	DPrintf("Node %d appending entries: %v", rf.me, args.Entries)
	rf.log = rf.log[:args.PrevLogIndex+1]
	rf.log = append(rf.log, args.Entries...)
	rf.persist()

	if args.LeaderCommit > rf.commitIndex {
		DPrintf("Node %d updating commitIndex", rf.me)
		DPrintf("Node %d commitIndex: %d, LeaderCommit: %d, log length-1: %d", rf.me, rf.commitIndex, args.LeaderCommit, len(rf.log)-1)
		if args.LeaderCommit < len(rf.log)-1 {
			rf.commitIndex = args.LeaderCommit
		} else {
			rf.commitIndex = len(rf.log) - 1
		}
		DPrintf("Node %d updated commitIndex to %d", rf.me, rf.commitIndex)
		rf.applyLogEntries()
	}

	reply.Success = true
	reply.Term = rf.currentTerm
}

func (rf *Raft) stepDown(replyTerm int) {
	DPrintf("Node %d updating term from %d to %d and stepping down", rf.me, rf.currentTerm, replyTerm)
	rf.isLeader = false
	rf.currentTerm = replyTerm
	rf.votedFor = -1
	rf.electionTimeoutReset()
	rf.persist()
}

func (rf *Raft) electionTimeoutReset() {
	select {
	case rf.resetElectionTimeoutCh <- struct{}{}:
	default:
	}
}

func (rf *Raft) electionTimeoutLoop() {
	for !rf.killed() {
		rf.mu.Lock()
		if rf.isLeader {
			rf.mu.Unlock()
			continue
		}
		rf.mu.Unlock()
		select {
		case <-time.After(rf.electionTimeout):
			rf.mu.Lock()
			if !rf.isLeader {
				DPrintf("Node %d election timeout, starting election", rf.me)
				rf.startElection()
			}
			rf.mu.Unlock()
		case <-rf.resetElectionTimeoutCh:
			DPrintf("Node %d resetting election timeout", rf.me)
		}
	}
}

func (rf *Raft) killed() bool {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.isKilled
}

func (rf *Raft) sendAppendEntries(server int, args AppendEntriesArgs, reply *AppendEntriesReply) bool {
	DPrintf("Node %d sending AppendEntries to %d for term %d", rf.me, server, args.Term)
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

func (rf *Raft) sendRequestVote(server int, args RequestVoteArgs, reply *RequestVoteReply) bool {
	DPrintf("Node %d sending RequestVote to %d for term %d", rf.me, server, args.Term)
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.isLeader {
		rf.log = append(rf.log, LogEntry{Term: rf.currentTerm, Command: command})
		rf.persist()
		DPrintf("Leader %d starting agreement for command %v at index %d, term %d", rf.me, command, len(rf.log)-1, rf.currentTerm)
		DPrintf("Leader %d log: %v", rf.me, rf.log)
		return len(rf.log) - 1, rf.currentTerm, true
	}

	return -1, rf.currentTerm, false
}

func (rf *Raft) applyLogEntries() {
	for rf.lastApplied < rf.commitIndex {
		rf.lastApplied++

		msg := ApplyMsg{
			Index:   rf.lastApplied,
			Command: rf.log[rf.lastApplied].Command,
		}

		DPrintf("Node %d  x %d: %v", rf.me, rf.lastApplied, msg)
		rf.mu.Unlock()
		rf.applyCh <- msg
		rf.mu.Lock()
	}
}

func (rf *Raft) Kill() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.isKilled = true
	return
}

func Make(peers []*labrpc.ClientEnd, me int, persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	rf.currentTerm = 0
	rf.votedFor = -1
	rf.isLeader = false
	rf.isKilled = false
	rf.electionTimeout = time.Duration(300+rand.Intn(50)) * time.Millisecond
	rf.resetElectionTimeoutCh = make(chan struct{}, 1)

	rf.log = make([]LogEntry, 0)
	rf.commitIndex = 0
	rf.lastApplied = 0

	rf.nextIndex = make([]int, len(peers))
	rf.matchIndex = make([]int, len(peers))
	rf.applyCh = applyCh

	rf.readPersist(persister.ReadRaftState())

	go rf.electionTimeoutLoop()

	return rf
}


