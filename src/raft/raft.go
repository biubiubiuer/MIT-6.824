package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	"6.824/labgob"
	"6.824/mr"
	"bytes"
	"math/rand"
	"sort"

	//	"bytes"
	"sync"
	"sync/atomic"
	"time"

	//	"6.824/labgob"
	"6.824/labrpc"
)

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in part 2D you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh, but set CommandValid to false for these
// other uses.
//
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
	CommandTerm  int

	// For 2D:
	SnapshotValid bool
	Snapshot      []byte
	SnapshotTerm  int
	SnapshotIndex int
}

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	// Your data here (2A, 2B, 2C).
	// Look at the paper's Figure 2 for a description of what
	// state a Raft server must maintain.

	nPeers int

	timeoutInterval time.Duration // follower leader candidate
	lastActiveTime  time.Time     // 超时开始计算时间, 收到心跳时会更新

	// 提交情况
	term     int
	role     MemberRole
	leaderId int
	voteFor  int

	// 提交情况
	logs        []*LogEntry
	nextIndex   []int
	matchIndex  []int
	commitIndex int
	lastApplied int

	lastIncludeIndex int // 快照的最大logIndex
	lastIncludeTerm  int // 最后一条压缩日志的term, 不是压缩时node's term
	snapshotOffset   int // 快照可能分批次传输
	snapshot         []byte

	// 状态机
	applyCond *sync.Cond
	applyChan chan ApplyMsg
}

type MemberRole int

const (
	Leader    MemberRole = 1
	Follower  MemberRole = 2
	Candidate MemberRole = 3

	RoleNone = -1
	None     = 0
)

func (m MemberRole) String() string {
	switch m {
	case Leader:
		return "Leader"
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	}
	return "Unknown"
}

type LogEntry struct {
	LogTerm int
	Command interface{}
}

const (
	ElectionTimeout  = 200 * time.Millisecond
	HeartBeatTimeout = 150 * time.Millisecond
)

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {

	// Your code here (2A).
	rf.mu.Lock()
	defer rf.mu.Unlock()

	return rf.term, rf.role == Leader
}

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
//

// 外层加锁, 内层不能够再加锁了
func (rf *Raft) persist() {
	// Your code here (2C).
	// Example:
	// w := new(bytes.Buffer)
	// e := labgob.NewEncoder(w)
	// e.Encode(rf.xxx)
	// e.Encode(rf.yyy)
	// data := w.Bytes()
	// rf.persister.SaveRaftState(data)

	rf.persister.SaveRaftState(rf.encodeRaftState())
}

func (rf *Raft) encodeRaftState() []byte {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	//持久化当前term以及是否给其他结点投过票，避免同一个term多次投票的情况
	e.Encode(rf.term)
	e.Encode(rf.voteFor)
	e.Encode(rf.leaderId)
	e.Encode(rf.logs)
	e.Encode(rf.lastIncludeIndex)
	return w.Bytes()
}

//
// restore previously persisted state.
//
// 一般刚刚启动时执行
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	// Your code here (2C).
	// Example:
	// r := bytes.NewBuffer(data)
	// d := labgob.NewDecoder(r)
	// var xxx
	// var yyy
	// if d.Decode(&xxx) != nil ||
	//    d.Decode(&yyy) != nil {
	//   error...
	// } else {
	//   rf.xxx = xxx
	//   rf.yyy = yyy
	// }

	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)

	rf.mu.Lock()
	d.Decode(&rf.term)
	d.Decode(&rf.voteFor)
	d.Decode(&rf.leaderId)
	d.Decode(&rf.logs)
	d.Decode(&rf.lastIncludeIndex)
	rf.lastIncludeTerm = rf.logs[0].LogTerm
	rf.mu.Unlock()
}

//
// A service wants to switch to snapshot.  Only do so if Raft hasn't
// have more recent info since it communicate the snapshot on applyCh.
//

// 应用层调用, 询问raft是否需要安装这个snapshot, 在InstallSnapshot时并不会安装
func (rf *Raft) CondInstallSnapshot(lastIncludedTerm int, lastIncludedIndex int, snapshot []byte) bool {

	// Your code here (2D).

	rf.mu.Lock()
	defer rf.mu.Unlock()
	//异步应用快照，如果此时commitIndex已经追上来了，就不需要再应用快照了
	if rf.commitIndex > lastIncludedIndex {
		return false
	}
	logs := rf.logs[0:1]
	logs[0].LogTerm = lastIncludedTerm
	//本结点最后一条日志在快照点之前，太落后，清空，应用快照，否则截断
	if rf.lastLogIndex() > lastIncludedIndex {
		logs = append(logs, rf.logs[lastIncludedIndex-rf.lastIncludeIndex+1:]...)
	}
	rf.logs = logs
	rf.snapshot = snapshot
	rf.lastIncludeIndex = lastIncludedIndex
	rf.lastIncludeTerm = lastIncludedTerm

	rf.lastApplied = lastIncludedIndex
	rf.commitIndex = lastIncludedIndex

	rf.persister.SaveStateAndSnapshot(rf.encodeRaftState(), snapshot)

	return true
}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.

// snapshot是应用层状态机的序列化, index表示checkpoint
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	// Your code here (2D).

	rf.mu.Lock()
	defer rf.mu.Unlock()
	if index <= rf.lastIncludeIndex || index != rf.lastApplied || index > rf.lastLogIndex() {
		return
	}
	DPrintf("node[%d] role[%d] term[%d] snapshoting, index[%d] commitIndex[%d] lastApplied[%d]", rf.me, rf.role, rf.term, index, rf.commitIndex, rf.lastApplied)
	logs := rf.logs[0:1]
	logs[0].LogTerm = rf.logs[index-rf.lastIncludeIndex].LogTerm
	//本结点最后一条日志在快照点之前，太落后，清空，应用快照，否则截断
	logs = append(logs, rf.logs[index-rf.lastIncludeIndex+1:]...)
	rf.logs = logs
	rf.snapshot = snapshot
	rf.lastIncludeIndex = index
	rf.lastIncludeTerm = logs[0].LogTerm
	rf.persister.SaveStateAndSnapshot(rf.encodeRaftState(), snapshot)

}

//
// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//

// 选举时需要传递自己拥有的最后一条log的term和index
type RequestVoteArgs struct {
	// Your data here (2A, 2B).
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	// Your data here (2A).
	Term        int
	VoteGranted bool
}

//
// example RequestVote RPC handler.
//
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	// Your code here (2A, 2B).
	rf.mu.Lock()
	defer func() {
		DPrintf("node[%d] role[%v] received vote request from node[%d], now[%d], args: %v, reply: %v", rf.me, rf.role, args.CandidateId, time.Now().UnixMilli(), mr.Any2String(args), mr.Any2String(reply))
	}()
	defer rf.mu.Unlock()

	reply.Term = rf.term
	reply.VoteGranted = false
	// 不接受小于自己的term的请求
	if rf.term > args.Term {
		return
	}

	if args.Term > rf.term {
		rf.role = Follower // leader -> follower
		rf.term = args.Term
		// 需要比较最新一条日志的情况再决定要不要投票
		rf.voteFor = RoleNone
		rf.leaderId = RoleNone
		rf.persist()
	}

	// 避免重复投票
	if rf.voteFor == RoleNone || rf.voteFor == args.CandidateId {
		lastLogTerm, lastLogIndex := rf.lastLogTermAndLastLogIndex()
		// 最后一条日志任期更大或者任期一样但是更长
		if args.LastLogTerm > lastLogTerm || (lastLogTerm == args.LastLogTerm && lastLogIndex <= args.LastLogIndex) {
			rf.role = Follower
			rf.voteFor = args.CandidateId
			rf.leaderId = args.CandidateId
			rf.lastActiveTime = time.Now()
			rf.timeoutInterval = randElectionTimeout()
			reply.VoteGranted = true
			rf.persist()
		}
	}
}

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//

// 写入数据
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	//index := -1
	//term := -1
	//isLeader := true

	// Your code here (2B).

	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.role != Leader {
		return -1, -1, false
	}
	entry := &LogEntry{
		LogTerm: rf.term,
		Command: command,
	}
	rf.logs = append(rf.logs, entry)
	index := rf.lastLogIndex()
	term := rf.term
	//写入后立刻持久化
	rf.persist()
	//DPrintf("node[%d] term[%d] role[%v] add entry: %v, logIndex[%d]", rf.me, rf.term, rf.role, mr.Any2String(entry), index)
	return index, term, true
}

//
// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
//
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

// The ticker go routine starts a new election if this peer hasn't received
// heartsbeats recently.
//func (rf *Raft) ticker() {
//	for rf.killed() == false {
//
//		// Your code here to check if a leader election should
//		// be started and to randomize sleeping time using
//		// time.Sleep().
//
//	}
//}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//

// 初始化raft, 所有raft的任务都要另起协程, 测试文件采用的是协程模拟rpc
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {

	rf := &Raft{
		mu:        sync.Mutex{},
		peers:     peers,
		persister: persister,
		me:        me,
		dead:      -1,
		nPeers:    len(peers),

		leaderId:        RoleNone,
		term:            None,
		voteFor:         RoleNone,
		role:            Follower,
		lastActiveTime:  time.Now(),
		timeoutInterval: randElectionTimeout(),
		commitIndex:     None,
		lastApplied:     None,
		applyChan:       applyCh,
	}
	rf.applyCond = sync.NewCond(&rf.mu)
	DPrintf("starting new raft node, id[%d], lastActiveTime[%v], timeoutInterval[%d]", me, rf.lastActiveTime.UnixMilli(), rf.timeoutInterval.Milliseconds())

	// Your initialization code here (2A, 2B, 2C).
	rf.logs = make([]*LogEntry, 0)
	rf.nextIndex = make([]int, rf.nPeers)
	rf.matchIndex = make([]int, rf.nPeers)

	for i := 0; i < rf.nPeers; i++ {
		rf.nextIndex[i] = 1
		rf.matchIndex[i] = 0
	}

	rf.logs = append(rf.logs, &LogEntry{
		LogTerm: 0,
	})

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	// start ticker goroutine to start elections
	//go rf.ticker()
	go rf.electionLoop()
	go rf.heartBeatLoop()
	go rf.applyLogLoop(applyCh)

	DPrintf("starting raft node[%d]", rf.me)

	return rf
}

func randElectionTimeout() time.Duration {
	return ElectionTimeout + time.Duration(rand.Uint32())%ElectionTimeout
}

func (rf *Raft) lastLogTermAndLastLogIndex() (int, int) {
	logIndex := rf.lastLogIndex()
	logTerm := rf.logs[logIndex-rf.lastIncludeIndex].LogTerm
	return logTerm, logIndex
}

func (rf *Raft) lastLogIndex() int {
	return len(rf.logs) - 1 + rf.lastIncludeIndex
}

// 三个协程

func (rf *Raft) electionLoop() {
	for rf.killed() == false {
		time.Sleep(time.Millisecond * 1)
		func() {
			rf.mu.Lock()
			defer rf.mu.Unlock()

			if rf.role == Leader {
				return
			}

			// 不超时不需要进入下一步, 只需要接收RequestVote和AppliedEntries请求即可
			if time.Now().Sub(rf.lastActiveTime) < rf.timeoutInterval {
				return
			}

			// 超时处理逻辑
			if rf.role == Follower {
				rf.role = Candidate
			}

			rf.lastActiveTime = time.Now()
			rf.timeoutInterval = randElectionTimeout()
			rf.voteFor = rf.me
			rf.term++
			rf.persist()
			lastLogTerm, lastLogIndex := rf.lastLogTermAndLastLogIndex()
			rf.mu.Unlock()

			maxTerm, voteGranted := rf.becomeCandidate(lastLogIndex, lastLogTerm)
			rf.mu.Lock()

			// 在这过程中接收到更大term的请求, 导致退化为follower
			if rf.role != Candidate {
				return
			}

			if maxTerm > rf.term {
				rf.role = Follower
				rf.term = maxTerm
				rf.voteFor = RoleNone
				rf.leaderId = RoleNone
				rf.persist()
			} else if voteGranted > rf.nPeers/2 {
				rf.leaderId = rf.me
				rf.role = Leader
				rf.lastActiveTime = time.Unix(0, 0)
				rf.persist()
			}

		}()
	}
}

func (rf *Raft) becomeCandidate(lastLogIndex, lastLogTerm int) (int, int) {

	type RequestVoteResult struct {
		peerId int
		resp   *RequestVoteReply
	}
	voteChan := make(chan *RequestVoteResult, rf.nPeers-1)
	args := &RequestVoteArgs{
		Term:         rf.term,
		CandidateId:  rf.me,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}

	for i := 0; i < rf.nPeers; i++ {
		if rf.me == i {
			continue
		}

		go func(server int, args *RequestVoteArgs) {
			reply := &RequestVoteReply{}
			ok := rf.sendRequestVote(server, args, reply)
			if ok {
				voteChan <- &RequestVoteResult{
					peerId: server,
					resp:   reply,
				}
			} else {
				voteChan <- &RequestVoteResult{
					peerId: server,
					resp:   nil,
				}
			}
		}(i, args)
	}

	maxTerm := rf.term
	voteGranted := 1
	totalVote := 1
	for i := 0; i < rf.nPeers-1; i++ {
		select {
		case vote := <-voteChan:
			totalVote++
			if vote.resp != nil {
				if vote.resp.VoteGranted {
					voteGranted++
				}
				// 出现更大term就退回follower
				if vote.resp.Term > maxTerm {
					maxTerm = vote.resp.Term
				}
			}
		}
		if voteGranted > rf.nPeers/2 || totalVote == rf.nPeers {
			return maxTerm, voteGranted
		}
	}

	return maxTerm, voteGranted
}

func (rf *Raft) heartBeatLoop() {
	for rf.killed() == false {
		// 改成10ms一次就通过了
		time.Sleep(time.Millisecond * 20)
		func() {
			rf.mu.Lock()
			defer rf.mu.Unlock()

			if rf.role != Leader {
				return
			}

			// 如果没有超时或者没有需要发送的数据, 则直接返回
			if time.Now().Sub(rf.lastActiveTime) < HeartBeatTimeout-50 {
				return
			}

			rf.lastActiveTime = time.Now()
			rf.matchIndex[rf.me] = rf.lastLogIndex()
			rf.nextIndex[rf.me] = rf.matchIndex[rf.me] + 1

			for i := 0; i < rf.nPeers; i++ {
				if rf.me == i {
					continue
				}

				// 日志在快照点之前, 发送快照
				if rf.nextIndex[i] <= rf.lastIncludeIndex {
					argsI := &InstallSnapshotArgs{
						Term:             rf.term,
						LeaderId:         rf.me,
						LastIncludeIndex: rf.lastIncludeIndex,
						LastIncludeTerm:  rf.lastIncludeTerm,
						Data:             rf.snapshot,
					}

					go func(server int, args *InstallSnapshotArgs) {
						reply := &InstallSnapshotReply{}
						ok := rf.sendInstallSnapshot(server, args, reply)
						if !ok {
							return
						}

						rf.mu.Lock()
						defer rf.mu.Unlock()

						if rf.term != args.Term || rf.role != Leader {
							return
						}

						// 发现更大的term, 本节点是旧leader
						if reply.Term > rf.term {
							rf.term = reply.Term
							rf.voteFor = RoleNone
							rf.leaderId = RoleNone
							rf.role = Follower
							rf.persist()
							return
						}

						// follower拒绝snapshot证明其commitIndex>lastIncludeIndex, 接收也可以使得其commitIndex>lastIncludeIndex
						rf.matchIndex[server] = rf.lastIncludeIndex
						rf.nextIndex[server] = rf.matchIndex[server] + 1

						matchIndexSlice := make([]int, rf.nPeers)
						for index, matchIndex := range rf.matchIndex {
							matchIndexSlice[index] = matchIndex
						}
						sort.Slice(matchIndexSlice, func(i, j int) bool {
							return matchIndexSlice[i] < matchIndexSlice[j]
						})

						newCommitIndex := matchIndexSlice[rf.nPeers/2]

						// 不能提交不属于当前term的日志
						if newCommitIndex > rf.commitIndex && rf.logTerm(newCommitIndex-rf.lastIncludeIndex) == rf.term {
							// 如果commitIndex比自己实际的日志长度还大, 这时需要减小
							if newCommitIndex > rf.lastLogIndex() {
								rf.commitIndex = rf.lastLogIndex()
							} else {
								rf.commitIndex = newCommitIndex
							}
						}
					}(i, argsI)
				} else {
					// 记录每个node本次发送日志的前一条日志
					prevLogIndex := rf.matchIndex[i]
					if prevLogIndex > rf.lastLogIndex() {
						prevLogIndex = rf.lastLogIndex()
					}

					// 发送日志
					// 有可能follower的matchIndex比leader还大, 此时要担心是否越界
					argsI := &AppendEntriesArgs{
						LogType:              HeartBeatLogType,
						Term:                 rf.term,
						LeaderId:             rf.me,
						PrevLogIndex:         prevLogIndex,
						PrevLogTerm:          rf.logTerm(prevLogIndex - rf.lastIncludeIndex),
						LeaderCommittedIndex: rf.commitIndex, //对上一次日志复制请求的二阶段
					}

					// 本次复制的最后一条日志
					if rf.matchIndex[i] < rf.lastLogIndex() {
						argsI.LogType = AppendEntryLogType
						argsI.LogEntries = make([]*LogEntry, 0)
						// 因为此时没有加锁, 担心又新日志写入, 必须保证每个节点复制的最后一条日志一样才能达到半提交的效果
						argsI.LogEntries = append(argsI.LogEntries, rf.logs[rf.nextIndex[i]-rf.lastIncludeIndex:]...)
					}

					go func(server int, args *AppendEntriesArgs) {
						reply := &AppendEntriesReply{}
						ok := rf.sendAppendEntries(server, args, reply)
						if !ok {
							return
						}

						rf.mu.Lock()
						defer rf.mu.Unlock()

						// 如果term变了, 表示该节点不再是leader, 什么也不做
						if rf.term != args.Term {
							return
						}

						// 发现更大的term, 本节点是旧leader
						if reply.Term > rf.term {
							rf.term = reply.Term
							rf.voteFor = RoleNone
							rf.leaderId = RoleNone
							rf.role = Follower
							rf.persist()
							return
						}

						// follower缺少之前的日志, 探测缺少的位置
						// 后退策略, 可以按term检测, 也可以二分, 此处采用线性探测, 简单一些
						rf.nextIndex[server] = reply.NextIndex
						rf.matchIndex[server] = reply.NextIndex - 1
						if reply.Success {
							// 提交到哪个位置需要根据中位数来判断, 中位数表示过半提交的日志位置,
							// 每次提交日志向各节点发送的日志并不完全一样, 不能光靠是否发送成功来判断
							matchIndexSlice := make([]int, rf.nPeers)
							for index, matchIndex := range rf.matchIndex {
								matchIndexSlice[index] = matchIndex
							}

							sort.Slice(matchIndexSlice, func(i, j int) bool {
								return matchIndexSlice[i] < matchIndexSlice[j]
							})

							newCommitIndex := matchIndexSlice[rf.nPeers/2]

							// 不能提交不属于当前term的日志
							DPrintf("sengAppendEntries id[%d] role[%v] appendEntries to node[%d] , lastLogIndex %v commitIndex %v update to newcommitIndex %v, lastSnapshotIndex %v, lastSnapshotTerm %d,  command: %v, matchIndex: %v\n", rf.me, rf.role, server, rf.lastLogIndex(), rf.commitIndex, newCommitIndex, rf.lastIncludeIndex, rf.lastIncludeTerm, 0, mr.Any2String(rf.matchIndex))

							// 只能往大更新
							if newCommitIndex > rf.commitIndex && rf.logTerm(newCommitIndex-rf.lastIncludeIndex) == rf.term {
								// 如果commitIndex比自己实际的日志长度还大, 这时需要减小
								if newCommitIndex > rf.lastLogIndex() {
									rf.commitIndex = rf.lastLogIndex()
								} else {
									rf.commitIndex = newCommitIndex
								}
							}
						}
					}(i, argsI)
				}
			}
		}()
	}
}

type InstallSnapshotArgs struct {
	Term             int // leader's term
	LeaderId         int
	LastIncludeIndex int // snapshot中最后一条日志的index
	LastIncludeTerm  int
	Data             []byte
}

type InstallSnapshotReply struct {
	Term int
}

// 心跳或者日志追加
type AppendEntriesArgs struct {
	LogType  LogType
	LeaderId int
	Term     int // leader currentTerm

	// 用于日志复制, 确保前面日志能够匹配
	PrevLogTerm          int
	PrevLogIndex         int
	LeaderCommittedIndex int
	LogEntries           []*LogEntry
}

type LogType int

func (rf *Raft) sendInstallSnapshot(server int, args *InstallSnapshotArgs, reply *InstallSnapshotReply) bool {
	ok := rf.peers[server].Call("Raft.InstallSnapshot", args, reply)
	return ok
}

func (rf *Raft) logTerm(logIndex int) int {
	return rf.logs[logIndex].LogTerm
}

const (
	HeartBeatLogType   LogType = 1
	AppendEntryLogType LogType = 2
)

func (l LogType) String() string {
	switch l {
	case HeartBeatLogType:
		return "HeartBeatLogType"
	case AppendEntryLogType:
		return "AppendEntryLogType"
	}
	return "Unknown"
}

// 心跳或日志追加
type AppendEntriesReply struct {
	Success bool
	Term    int

	// 用于探测日志匹配点
	NextIndex int
	Msg       string
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

// 如果收到term比自己大的AppendEntries请求, 则表示发生新一轮的选举, 此时拒绝掉, 等待超时选举
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {

	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer func() {
		DPrintf("process AppendEntries node[%d] role[%v] from node[%d] term[%d] args: %v, reply: %v", rf.me, rf.role, args.LeaderId, args.Term, mr.Any2String(args), mr.Any2String(reply))
	}()

	reply.Term = rf.term
	reply.Success = false
	// 拒绝旧leader请求
	if args.Term < rf.term {
		DPrintf("appendEntries node[%v] term[%d] from node[%v] term[%d], args.Term < rf.term declined", rf.me, rf.term, args.LeaderId, args.Term)
		return
	}
	// 发现一个更大的任期，转变成这个term的follower leader follower--> follower
	if args.Term > rf.term {
		rf.term = args.Term
		rf.role = Follower
		//发现term大于等于自己的日志复制请求，则认其为主
		rf.voteFor = RoleNone
		rf.leaderId = RoleNone
		rf.persist()
	}
	rf.leaderId = args.LeaderId
	rf.voteFor = args.LeaderId
	rf.lastActiveTime = time.Now()
	// 还缺少前面的日志或者前一条日志匹配不上
	if args.PrevLogIndex > rf.lastLogIndex() {
		reply.NextIndex = rf.lastLogIndex() + 1
		DPrintf("appendEntries node[%d] term[%d] lastLogIndex[%d] from node[%d], args.PrevLogIndex > rf.lastLogIndex() declined, args: %v reply: %v ", rf.me, rf.term, rf.lastLogIndex(), args.LeaderId, mr.Any2String(args), mr.Any2String(reply))
		return
	}
	// 本peer压缩过, 不能再往前探测, 压缩的日志一定是已经提交的日志
	if args.PrevLogIndex < rf.lastIncludeIndex {
		reply.NextIndex = rf.lastIncludeIndex + 1
		// nextIndex向后移动了就算success
		reply.Success = false
		return
	}
	if args.PrevLogTerm != rf.logTerm(args.PrevLogIndex-rf.lastIncludeIndex) {
		// 前一条日志的任期不匹配, 找到冲突term首次出现的地方
		index := args.PrevLogIndex - rf.lastIncludeIndex
		term := rf.logTerm(index)
		for ; index > 0 && rf.logTerm(index) == term; index-- {
		}
		reply.NextIndex = index + 1 + rf.lastIncludeIndex
		DPrintf("AppendEntries node[%v] term[%d] lastIncludedIndex[%d] lastIncludedTerm[%d]  from node[%v] term[%d], args.PrevLogTerm != rf.logTerm(args.PrevLogIndex-rf.lastIncludeIndex) declined, reply: %v, args: %v", rf.me, rf.term, rf.lastIncludeIndex, rf.lastIncludeTerm, args.LeaderId, args.Term, mr.Any2String(reply), mr.Any2String(args))
		return
	}
	// args.PrevLogIndex<=lastLogIndex, 有可能发生截断的情况
	if rf.lastLogIndex() > args.PrevLogIndex {
		rf.logs = rf.logs[:args.PrevLogIndex+1-rf.lastIncludeIndex]
	}
	rf.logs = append(rf.logs, args.LogEntries...)
	if args.LeaderCommittedIndex > rf.commitIndex {
		commitIndex := args.LeaderCommittedIndex
		// 结点可能很落后
		if rf.lastLogIndex() < commitIndex {
			commitIndex = rf.lastLogIndex()
		}
		rf.commitIndex = commitIndex
	}
	rf.persist()
	reply.Success = true
	rf.matchIndex[rf.me] = rf.lastLogIndex()
	rf.nextIndex[rf.me] = rf.matchIndex[rf.me] + 1
	reply.NextIndex = rf.nextIndex[rf.me]
}

func (rf *Raft) applyLogLoop(applyCh chan ApplyMsg) {
	for !rf.killed() {
		time.Sleep(time.Millisecond * 10)
		var applyMsg *ApplyMsg
		logSize := 0
		func() {
			rf.mu.Lock()
			defer rf.mu.Unlock()

			logSize = len(rf.logs) - 1

			// 没有数据需要上传给应用层
			if rf.lastApplied >= rf.commitIndex {
				return
			}

			if rf.lastApplied < rf.lastIncludeIndex {
				rf.lastApplied = rf.lastIncludeIndex
			}

			if rf.lastApplied < rf.commitIndex {
				rf.lastApplied++
				applyMsg = &ApplyMsg{
					CommandValid: true,
					Command:      rf.logEntry(rf.lastApplied).Command,
					CommandIndex: rf.lastApplied,
					CommandTerm:  rf.logEntry(rf.lastApplied).LogTerm,
				}
			}
		}()

		if applyMsg != nil {
			go func() {
				// 锁外提交给应用
				applyCh <- *applyMsg
				DPrintf("id[%v] role[%v] upload log to application, lastApplied[%d], commitIndex[%d], logSize[%d]", rf.me, rf.role, applyMsg.CommandIndex, rf.commitIndex, logSize)
			}()
		}
	}
}

func (rf *Raft) logEntry(logIndex int) *LogEntry {
	// 越界
	if logIndex > rf.lastLogIndex() {
		return rf.logs[0] // fuck index
	}

	// 该日志已被压缩
	logIndex = logIndex - rf.lastIncludeIndex
	if logIndex <= 0 {
		return rf.logs[0] // fuck index
	}
	return rf.logs[logIndex]
}
