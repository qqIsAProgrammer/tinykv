// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"math/rand"
	"sort"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64

	// randomizedElectionTimeout is a random number between
	// [electiontimeout, 2 * electiontimeout - 1]. It gets reset
	// when raft changes its state to follower or candidate.
	randomizedElectionTimeout int
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	r := &Raft{
		id:               c.ID,
		RaftLog:          newLog(c.Storage),
		Prs:              make(map[uint64]*Progress),
		votes:            make(map[uint64]bool),
		electionTimeout:  c.ElectionTick,
		heartbeatTimeout: c.HeartbeatTick,
	}

	hardSt, confSt, _ := c.Storage.InitialState()

	if c.peers == nil {
		c.peers = confSt.Nodes
	}
	for _, peer := range c.peers {
		r.Prs[peer] = &Progress{}
	}

	r.becomeFollower(0, None)
	r.resetRandomizedElectionTimeout()
	r.Term, r.Vote, r.RaftLog.committed = hardSt.Term, hardSt.Vote, hardSt.Commit
	if c.Applied > 0 {
		r.RaftLog.applied = c.Applied
	}
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	prevLogIndex := r.Prs[to].Next - 1
	prevLogTerm, err := r.RaftLog.Term(prevLogIndex)
	if err != nil {
		if err == ErrCompacted {
			// TODO: send snapshot
			return false
		}
		panic(err)
	}

	var ents []*pb.Entry
	start := r.RaftLog.toSliceIndex(prevLogIndex + 1)
	n := len(r.RaftLog.entries)
	for i := start; i < n; i++ {
		ents = append(ents, &r.RaftLog.entries[i])
	}

	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		LogTerm: prevLogTerm,
		Index:   prevLogIndex,
		Entries: ents,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, msg)

	return true
}

func (r *Raft) sendAppendResponse(to, logTerm, index uint64, reject bool) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		LogTerm: logTerm,
		Index:   index,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		To:      to,
		From:    r.id,
		Term:    r.Term,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendHeartbeatResponse(to uint64, reject bool) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendRequestVote(to, index, term uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		LogTerm: term,
		Index:   index,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendRequestVoteResponse(to uint64, reject bool) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		To:      to,
		From:    r.id,
		Term:    r.Term,
		Reject:  reject,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) bcastHeartbeat() {
	for peer := range r.Prs {
		if r.id != peer {
			r.sendHeartbeat(peer)
		}
	}
}

func (r *Raft) bcastAppend() {
	for peer := range r.Prs {
		if r.id != peer {
			r.sendAppend(peer)
		}
	}
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	switch r.State {
	case StateFollower:
		r.tickElection()
	case StateCandidate:
		r.tickElection()
	case StateLeader:
		r.tickHeartbeat()
	}
}

func (r *Raft) tickElection() {
	r.electionElapsed++
	if r.electionElapsed >= r.randomizedElectionTimeout {
		r.electionElapsed = 0
		_ = r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
	}
}

func (r *Raft) tickHeartbeat() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		_ = r.Step(pb.Message{MsgType: pb.MessageType_MsgBeat})
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.State = StateFollower
	r.Term = term
	r.Lead = lead
	r.Vote = None
	r.resetRandomizedElectionTimeout()
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	r.Term++
	r.Lead = None
	r.Vote = r.id
	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true
	r.resetRandomizedElectionTimeout()
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.State = StateLeader
	r.Lead = r.id
	r.heartbeatElapsed = 0

	// Append a noop entry
	lastIndex := r.RaftLog.LastIndex()
	for peer := range r.Prs {
		if r.id == peer {
			r.Prs[peer].Next = lastIndex + 2
			r.Prs[peer].Match = lastIndex + 1
		} else {
			r.Prs[peer].Next = lastIndex + 1
		}
	}
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{Term: r.Term, Index: lastIndex + 1})
	r.bcastAppend()

	if len(r.Prs) == 1 {
		r.RaftLog.committed = r.Prs[r.id].Match
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	if _, ok := r.Prs[r.id]; !ok {
		return nil
	}

	if m.Term > r.Term {
		r.becomeFollower(m.Term, None)
	}
	switch r.State {
	case StateFollower:
		_ = r.stepFollower(m)
	case StateCandidate:
		_ = r.stepCandidate(m)
	case StateLeader:
		_ = r.stepLeader(m)
	}
	return nil
}

func (r *Raft) stepFollower(m pb.Message) error {
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.doElection()
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgSnapshot:
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgTransferLeader:
	case pb.MessageType_MsgTimeoutNow:
	}
	return nil
}

func (r *Raft) stepCandidate(m pb.Message) error {
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.doElection()
	case pb.MessageType_MsgAppend:
		if m.Term == r.Term {
			r.becomeFollower(m.Term, m.From)
		}
		r.handleAppendEntries(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handleRequestVoteResponse(m)
	case pb.MessageType_MsgSnapshot:
	case pb.MessageType_MsgHeartbeat:
		if m.Term == r.Term {
			r.becomeFollower(m.Term, m.From)
		}
		r.handleHeartbeat(m)
	case pb.MessageType_MsgTransferLeader:
	case pb.MessageType_MsgTimeoutNow:
	}
	return nil
}

func (r *Raft) stepLeader(m pb.Message) error {
	switch m.MsgType {
	case pb.MessageType_MsgBeat:
		r.bcastHeartbeat()
	case pb.MessageType_MsgPropose:
		r.appendEntries(m.Entries)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendEntriesResponse(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgSnapshot:
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
		if !m.Reject {
			r.sendAppend(m.From)
		}
	case pb.MessageType_MsgTransferLeader:
	case pb.MessageType_MsgTimeoutNow:
	}
	return nil
}

func (r *Raft) doElection() {
	r.becomeCandidate()
	r.heartbeatElapsed = 0
	if len(r.Prs) == 1 {
		r.becomeLeader()
		return
	}

	lastIndex := r.RaftLog.LastIndex()
	lastLogTerm, _ := r.RaftLog.Term(lastIndex)
	for peer := range r.Prs {
		if peer != r.id {
			r.sendRequestVote(peer, lastIndex, lastLogTerm)
		}
	}
}

func (r *Raft) handleRequestVote(m pb.Message) {
	// Q: Why `r.Term == m.Term` won't reject?
	// A: See `Step()`
	if r.Term > m.Term {
		r.sendRequestVoteResponse(m.From, true)
		return
	}

	if r.Vote == None || r.Vote == m.From {
		if r.RaftLog.isUpToDate(m.Index, m.LogTerm) {
			r.Vote = m.From
			r.electionElapsed = 0
			r.sendRequestVoteResponse(m.From, false)
			return
		}
	}
	r.sendRequestVoteResponse(m.From, true)
}

func (r *Raft) handleRequestVoteResponse(m pb.Message) {
	r.votes[m.From] = !m.Reject
	granted := 0
	quorum := len(r.Prs) / 2
	for _, v := range r.votes {
		if v {
			granted++
		}
	}
	if granted > quorum {
		r.becomeLeader()
	} else if len(r.votes)-granted > quorum {
		r.becomeFollower(r.Term, None)
	}
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if r.Term > m.Term {
		r.sendAppendResponse(m.From, None, None, true)
		return
	}

	r.Lead = m.From
	r.electionElapsed = 0

	lastIndex := r.RaftLog.LastIndex()
	if m.Index > lastIndex {
		r.sendAppendResponse(m.From, None, lastIndex+1, true)
		return
	}
	if m.Index >= r.RaftLog.FirstIndex() {
		logTerm, err := r.RaftLog.Term(m.Index)
		if err != nil {
			panic(err)
		}
		// Find the minimum log index at logTerm (index -> nextIndex)
		if logTerm != m.LogTerm {
			nexti := r.RaftLog.toEntryIndex(sort.Search(r.RaftLog.toSliceIndex(m.Index+1),
				func(i int) bool { return r.RaftLog.entries[i].Term == logTerm }))
			r.sendAppendResponse(m.From, logTerm, nexti, true)
			return
		}
	}

	for i, ent := range m.Entries {
		if ent.Index < r.RaftLog.FirstIndex() {
			continue
		}
		if ent.Index <= r.RaftLog.LastIndex() {
			logTerm, err := r.RaftLog.Term(ent.Index)
			if err != nil {
				panic(err)
			}
			if logTerm != ent.Term {
				idx := r.RaftLog.toSliceIndex(ent.Index)
				r.RaftLog.entries[idx] = *ent
				r.RaftLog.entries = r.RaftLog.entries[:idx+1]
				// Truncation maybe cause stabled index decrement
				r.RaftLog.stabled = min(r.RaftLog.stabled, ent.Index-1)
			}
		} else {
			for j := i; j < len(m.Entries); j++ {
				r.RaftLog.entries = append(r.RaftLog.entries, *m.Entries[j])
			}
			break
		}
	}

	if r.RaftLog.committed < m.Commit {
		// Q: Why use m.Index+uint64(len(m.Entries)) instead of r.RaftLog.LastIndex()?
		// A: If m.Entries is empty, it won't enter the loop, and maybe the commitIndex
		// is less than lastIndex.
		r.RaftLog.committed = min(m.Commit, m.Index+uint64(len(m.Entries)))
	}
	r.sendAppendResponse(m.From, None, r.RaftLog.LastIndex(), false)
}

func (r *Raft) handleAppendEntriesResponse(m pb.Message) {
	if r.Term > m.Term {
		return
	}

	if m.Reject {
		rejectHint := m.Index
		if rejectHint == None {
			return
		}
		if m.LogTerm != None {
			logTerm := m.LogTerm
			sliceIndex := sort.Search(len(r.RaftLog.entries),
				func(i int) bool { return r.RaftLog.entries[i].Term > logTerm })
			if sliceIndex > 0 && r.RaftLog.entries[sliceIndex-1].Term == logTerm {
				rejectHint = r.RaftLog.toEntryIndex(sliceIndex)
			}
		}
		r.Prs[m.From].Next = rejectHint
		r.sendAppend(m.From)
		return
	}

	if m.Index > r.Prs[m.From].Match {
		r.Prs[m.From].Match = m.Index
		r.Prs[m.From].Next = m.Index + 1
		r.leaderCommit()
	}
}

func (r *Raft) leaderCommit() {
	match := make(uint64Slice, len(r.Prs))
	i := 0
	for _, pr := range r.Prs {
		match[i] = pr.Match
		i++
	}
	sort.Sort(match)
	n := match[(len(match)-1)/2]

	if n > r.RaftLog.committed {
		logTerm, err := r.RaftLog.Term(n)
		if err != nil {
			panic(err)
		}
		if logTerm == r.Term {
			r.RaftLog.committed = n
			r.bcastAppend()
		}
	}
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if r.Term > m.Term {
		r.sendHeartbeatResponse(m.From, true)
		return
	}
	r.Lead = m.From
	r.electionElapsed = 0
	r.sendHeartbeatResponse(m.From, false)
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
}

func (r *Raft) appendEntries(ents []*pb.Entry) {
	lastIndex := r.RaftLog.LastIndex()
	for i, ent := range ents {
		ent.Term = r.Term
		ent.Index = lastIndex + uint64(i) + 1
		r.RaftLog.entries = append(r.RaftLog.entries, *ent)
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.Prs[r.id].Match + 1
	r.bcastAppend()

	if len(r.Prs) == 1 {
		r.RaftLog.committed = r.Prs[r.id].Match
	}
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}

func (r *Raft) softState() *SoftState {
	return &SoftState{
		Lead:      r.Lead,
		RaftState: r.State,
	}
}

func (r *Raft) hardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.RaftLog.committed,
	}
}

func (r *Raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
}
