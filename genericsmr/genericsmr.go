package genericsmr

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/glycerine/qlease/fastrpc"
	"github.com/glycerine/qlease/genericsmrproto"
	"github.com/glycerine/qlease/qlease"
	"github.com/glycerine/qlease/qleaseproto"
	"github.com/glycerine/qlease/rdtsc"
	"github.com/glycerine/qlease/state"
)

const CHAN_BUFFER_SIZE = 500000

const (
	TRUE  = uint8(1)
	FALSE = uint8(0)
)

type RPCPair struct {
	Obj  fastrpc.Serializable
	Chan chan fastrpc.Serializable
}

type Propose struct {
	*genericsmrproto.Propose
	FwdReplica int32
	FwdId      int32
	Writer     *bufio.Writer
	Lock       *sync.Mutex
}

type Beacon struct {
	Rid       int32
	Timestamp uint64
}

type Replica struct {
	N            int        // total number of replicas
	Id           int32      // the ID of the current replica
	PeerAddrList []string   // array with the IP:port address of every replica
	Peers        []net.Conn // cache of connections to all other replicas
	PeerReaders  []*bufio.Reader
	PeerWriters  []*bufio.Writer
	PeerWLocks   []*sync.Mutex
	Alive        []bool // connection status
	Listener     net.Listener

	State *state.State

	ProposeChan chan *Propose // channel for client proposals
	BeaconChan  chan *Beacon  // channel for beacons from peer replicas

	Shutdown bool

	Thrifty bool // send only as many messages as strictly required?
	Exec    bool // execute commands?
	Dreply  bool // reply to client after command has been executed?
	Beacon  bool // send beacons to detect how fast are the other replicas?

	Durable     bool     // log to a stable store?
	StableStore *os.File // file support for the persistent log

	PreferredPeerOrder []int32 // replicas in the preferred order of communication

	QLease                *qlease.Lease             // the latest quorum lease (nil if not initialized)
	QLPromiseChan         chan fastrpc.Serializable // channel for incoming quorum read lease promises
	QLPromiseReplyChan    chan fastrpc.Serializable // channel for incoming quorum read lease promise-replies
	qleasePromiseRPC      uint8
	qleasePromiseReplyRPC uint8
	QLGuardChan           chan fastrpc.Serializable
	QLGuardReplyChan      chan fastrpc.Serializable
	qleaseGuardRPC        uint8
	qleaseGuardReplyRPC   uint8

	Updating map[state.Key]bool // set of keys being updated (i.e., the current replica has received a
	// (Pre)Accept, for an update on that key, but not yet a Commit

	rpcTable map[uint8]*RPCPair
	rpcCode  uint8

	Ewma []float64

	OnClientConnect chan bool

	LastReplyReceivedTimestamp []int64
}

func NewReplica(id int, peerAddrList []string, thrifty bool, exec bool, dreply bool) *Replica {
	r := &Replica{
		len(peerAddrList),
		int32(id),
		peerAddrList,
		make([]net.Conn, len(peerAddrList)),
		make([]*bufio.Reader, len(peerAddrList)),
		make([]*bufio.Writer, len(peerAddrList)),
		make([]*sync.Mutex, len(peerAddrList)),
		make([]bool, len(peerAddrList)),
		nil,
		state.InitState(),
		make(chan *Propose, CHAN_BUFFER_SIZE),
		make(chan *Beacon, CHAN_BUFFER_SIZE),
		false,
		thrifty,
		exec,
		dreply,
		false,
		false,
		nil,
		make([]int32, len(peerAddrList)),
		nil,
		make(chan fastrpc.Serializable, 1000),
		make(chan fastrpc.Serializable, 1000),
		0,
		0,
		make(chan fastrpc.Serializable, 1000),
		make(chan fastrpc.Serializable, 1000),
		0,
		0,
		make(map[state.Key]bool, 10000),
		make(map[uint8]*RPCPair),
		genericsmrproto.GENERIC_SMR_BEACON_REPLY + 1,
		make([]float64, len(peerAddrList)),
		make(chan bool, 100),
		make([]int64, len(peerAddrList))}

	var err error

	if r.StableStore, err = os.Create(fmt.Sprintf("stable-store-replica%d", r.Id)); err != nil {
		log.Fatal(err)
	}

	for i := 0; i < r.N; i++ {
		r.PreferredPeerOrder[i] = int32((int(r.Id) + 1 + i) % r.N)
		r.Ewma[i] = 0.0
		r.PeerWLocks[i] = new(sync.Mutex)
		r.LastReplyReceivedTimestamp[i] = 0 //time.Now().UnixNano()
	}

	r.qleasePromiseRPC = r.RegisterRPC(new(qleaseproto.Promise), r.QLPromiseChan)
	r.qleasePromiseReplyRPC = r.RegisterRPC(new(qleaseproto.PromiseReply), r.QLPromiseReplyChan)
	r.qleaseGuardRPC = r.RegisterRPC(new(qleaseproto.Guard), r.QLGuardChan)
	r.qleaseGuardReplyRPC = r.RegisterRPC(new(qleaseproto.GuardReply), r.QLGuardReplyChan)

	return r
}

/* Client API */

func (r *Replica) Ping(args *genericsmrproto.PingArgs, reply *genericsmrproto.PingReply) error {
	return nil
}

func (r *Replica) BeTheLeader(args *genericsmrproto.BeTheLeaderArgs, reply *genericsmrproto.BeTheLeaderReply) error {
	return nil
}

/* ============= */

func (r *Replica) ConnectToPeers() {
	var b [4]byte
	bs := b[:4]
	done := make(chan bool)

	go r.waitForPeerConnections(done)

	//connect to peers
	for i := 0; i < int(r.Id); i++ {
		for done := false; !done; {
			if conn, err := net.Dial("tcp", r.PeerAddrList[i]); err == nil {
				r.Peers[i] = conn
				done = true
			} else {
				time.Sleep(1e9)
			}
		}
		binary.LittleEndian.PutUint32(bs, uint32(r.Id))
		if _, err := r.Peers[i].Write(bs); err != nil {
			fmt.Println("Write id error:", err)
			continue
		}
		r.Alive[i] = true
		r.PeerReaders[i] = bufio.NewReader(r.Peers[i])
		r.PeerWriters[i] = bufio.NewWriter(r.Peers[i])
	}
	<-done
	log.Printf("Replica id: %d. Done connecting to peers\n", r.Id)

	for rid, reader := range r.PeerReaders {
		if int32(rid) == r.Id {
			continue
		}
		go r.replicaListener(rid, reader)
	}
}

func (r *Replica) ConnectToPeersNoListeners() {
	var b [4]byte
	bs := b[:4]
	done := make(chan bool)

	go r.waitForPeerConnections(done)

	//connect to peers
	for i := 0; i < int(r.Id); i++ {
		for done := false; !done; {
			if conn, err := net.Dial("tcp", r.PeerAddrList[i]); err == nil {
				r.Peers[i] = conn
				done = true
			} else {
				time.Sleep(1e9)
			}
		}
		binary.LittleEndian.PutUint32(bs, uint32(r.Id))
		if _, err := r.Peers[i].Write(bs); err != nil {
			fmt.Println("Write id error:", err)
			continue
		}
		r.Alive[i] = true
		r.PeerReaders[i] = bufio.NewReader(r.Peers[i])
		r.PeerWriters[i] = bufio.NewWriter(r.Peers[i])
	}
	<-done
	log.Printf("Replica id: %d. Done connecting to peers\n", r.Id)
}

/* Peer (replica) connections dispatcher */
func (r *Replica) waitForPeerConnections(done chan bool) {
	var b [4]byte
	bs := b[:4]

	r.Listener, _ = net.Listen("tcp", r.PeerAddrList[r.Id])
	for i := r.Id + 1; i < int32(r.N); i++ {
		conn, err := r.Listener.Accept()
		if err != nil {
			fmt.Println("Accept error:", err)
			continue
		}
		if _, err := io.ReadFull(conn, bs); err != nil {
			fmt.Println("Connection establish error:", err)
			continue
		}
		id := int32(binary.LittleEndian.Uint32(bs))
		r.Peers[id] = conn
		r.PeerReaders[id] = bufio.NewReader(conn)
		r.PeerWriters[id] = bufio.NewWriter(conn)
		r.Alive[id] = true
	}

	done <- true
}

/* Client connections dispatcher */
func (r *Replica) WaitForClientConnections() {
	for !r.Shutdown {
		conn, err := r.Listener.Accept()
		if err != nil {
			log.Println("Accept error:", err)
			continue
		}
		go r.clientListener(conn)

		r.OnClientConnect <- true
	}
}

func (r *Replica) replicaListener(rid int, reader *bufio.Reader) {
	var msgType uint8
	var err error = nil
	var gbeacon genericsmrproto.Beacon
	var gbeaconReply genericsmrproto.BeaconReply

	for err == nil && !r.Shutdown {

		if msgType, err = reader.ReadByte(); err != nil {
			break
		}

		switch uint8(msgType) {

		case genericsmrproto.GENERIC_SMR_BEACON:
			if err = gbeacon.Unmarshal(reader); err != nil {
				break
			}
			beacon := &Beacon{int32(rid), gbeacon.Timestamp}
			r.BeaconChan <- beacon
			break

		case genericsmrproto.GENERIC_SMR_BEACON_REPLY:
			if err = gbeaconReply.Unmarshal(reader); err != nil {
				break
			}
			//TODO: UPDATE STUFF
			r.Ewma[rid] = 0.99*r.Ewma[rid] + 0.01*float64(rdtsc.Cputicks()-gbeaconReply.Timestamp)
			log.Println(r.Ewma)
			break

		default:
			if rpair, present := r.rpcTable[msgType]; present {
				obj := rpair.Obj.New()
				if err = obj.Unmarshal(reader); err != nil {
					break
				}
				rpair.Chan <- obj
			} else {
				log.Println("Error: received unknown message type")
			}
		}
	}
}

func (r *Replica) clientListener(conn net.Conn) {
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	lock := new(sync.Mutex)

	var msgType byte //:= make([]byte, 1)
	var err error
	for !r.Shutdown && err == nil {

		if msgType, err = reader.ReadByte(); err != nil {
			break
		}

		switch uint8(msgType) {

		case genericsmrproto.PROPOSE:
			prop := new(genericsmrproto.Propose)
			if err = prop.Unmarshal(reader); err != nil {
				break
			}
			r.ProposeChan <- &Propose{prop, -1, -1, writer, lock}
			break

		case genericsmrproto.READ:
			read := new(genericsmrproto.Read)
			if err = read.Unmarshal(reader); err != nil {
				break
			}
			//r.ReadChan <- read
			break

		case genericsmrproto.PROPOSE_AND_READ:
			pr := new(genericsmrproto.ProposeAndRead)
			if err = pr.Unmarshal(reader); err != nil {
				break
			}
			//r.ProposeAndReadChan <- pr
			break
		}
	}
	if err != nil && err != io.EOF {
		log.Println("Error when reading from client connection:", err)
	}
}

func (r *Replica) RegisterRPC(msgObj fastrpc.Serializable, notify chan fastrpc.Serializable) uint8 {
	code := r.rpcCode
	r.rpcCode++
	r.rpcTable[code] = &RPCPair{msgObj, notify}
	return code
}

var SendError bool

func (r *Replica) SendMsg(peerId int32, code uint8, msg fastrpc.Serializable) (retErr error) {
	defer func() {
		if err := recover(); err != nil {
			r.Alive[peerId] = false
			log.Println("Send Error: ", err)
			retErr = errors.New("Send Error")
			SendError = true
		}
	}()
	SendError = false
	if !r.Alive[peerId] {
		SendError = true
		return errors.New("Trying to send to a replica that may not be alive")
	}
	r.PeerWLocks[peerId].Lock()
	defer r.PeerWLocks[peerId].Unlock()
	w := r.PeerWriters[peerId]
	w.WriteByte(code)
	msg.Marshal(w)
	w.Flush()
	return nil
}

func (r *Replica) SendMsgNoFlush(peerId int32, code uint8, msg fastrpc.Serializable) (retErr error) {
	defer func() error {
		if err := recover(); err != nil {
			r.Alive[peerId] = false
			retErr = errors.New("SendNoFlush Error")
			SendError = true
		}
		return nil
	}()
	SendError = false
	if !r.Alive[peerId] {
		SendError = true
		return errors.New("Trying to send to a replica that may not be alive")
	}
	r.PeerWLocks[peerId].Lock()
	defer r.PeerWLocks[peerId].Unlock()
	w := r.PeerWriters[peerId]
	w.WriteByte(code)
	msg.Marshal(w)
	return nil
}

func (r *Replica) ReplyPropose(reply *genericsmrproto.ProposeReply, propose *Propose) {
	if propose.Writer == nil || propose.Lock == nil {
		return
	}
	propose.Lock.Lock()
	defer propose.Lock.Unlock()
	//w.WriteByte(genericsmrproto.PROPOSE_REPLY)
	reply.Marshal(propose.Writer)
	propose.Writer.Flush()
}

func (r *Replica) ReplyProposeTS(reply *genericsmrproto.ProposeReplyTS, propose *Propose) {
	if propose.Writer == nil || propose.Lock == nil {
		return
	}
	propose.Lock.Lock()
	defer propose.Lock.Unlock()
	//w.WriteByte(genericsmrproto.PROPOSE_REPLY)
	reply.Marshal(propose.Writer)
	propose.Writer.Flush()
}

func (r *Replica) SendBeacon(peerId int32) {
	w := r.PeerWriters[peerId]
	w.WriteByte(genericsmrproto.GENERIC_SMR_BEACON)
	beacon := &genericsmrproto.Beacon{rdtsc.Cputicks()}
	beacon.Marshal(w)
	w.Flush()
}

func (r *Replica) ReplyBeacon(beacon *Beacon) {
	w := r.PeerWriters[beacon.Rid]
	w.WriteByte(genericsmrproto.GENERIC_SMR_BEACON_REPLY)
	rb := &genericsmrproto.BeaconReply{beacon.Timestamp}
	rb.Marshal(w)
	w.Flush()
}

func (r *Replica) EstablishQLease(ql *qlease.Lease) {
	now := time.Now().UnixNano()
	ql.LatestTsSent = now
	ql.PromiseRejects = 0
	g := &qleaseproto.Guard{r.Id, now, qlease.GUARD_DURATION_NS}
	for i := int32(0); i < int32(r.N); i++ {
		if i == r.Id || !r.Alive[i] {
			continue
		}
		r.SendMsg(i, r.qleaseGuardRPC, g)
	}
}

func (r *Replica) RenewQLease(ql *qlease.Lease, latestAccInst int32) {
	now := time.Now().UnixNano()
	ql.PromiseRejects = 0
	p := &qleaseproto.Promise{r.Id, ql.PromisedByMeInst, now, ql.Duration, latestAccInst}
	for i := int32(0); i < int32(r.N); i++ {
		if i == r.Id || !r.Alive[i] {
			continue
		}
		ql.LatestRepliesReceived[i] += ql.Duration
		r.SendMsg(i, r.qleasePromiseRPC, p)
	}
	ql.LatestTsSent = now

	// sufficient to extend wait time by the duration of the lease, because
	// grantees must receive the lease refresh message before the previous lease expires
	// (otherwise they will dicount the refresh)
	ql.WriteInQuorumUntil += ql.Duration
}

type Int64Slice []int64

func (s Int64Slice) Len() int {
	return len(s)
}
func (s Int64Slice) Less(i, j int) bool {
	return s[i] < s[j]
}
func (s Int64Slice) Swap(i, j int) {
	aux := s[i]
	s[i] = s[j]
	s[j] = aux
}

func (r *Replica) HandleQLeaseGuard(ql *qlease.Lease, g *qleaseproto.Guard) {
	ql.GuardExpires[g.ReplicaId] = time.Now().UnixNano() + g.GuardDuration
	gr := &qleaseproto.GuardReply{r.Id, g.TimestampNs}
	r.SendMsg(g.ReplicaId, r.qleaseGuardReplyRPC, gr)
}

func (r *Replica) HandleQLeaseGuardReply(ql *qlease.Lease, gr *qleaseproto.GuardReply, latestAccInst int32) {

	if gr.TimestampNs < ql.LatestTsSent {
		//old reply, must ignore
		return
	}

	now := time.Now().UnixNano()

	p := &qleaseproto.Promise{r.Id, ql.PromisedByMeInst, now, ql.Duration, latestAccInst}

	ql.LatestRepliesReceived[gr.ReplicaId] = now + qlease.GUARD_DURATION_NS + ql.Duration

	if ql.WriteInQuorumUntil < ql.LatestRepliesReceived[gr.ReplicaId] {
		ql.WriteInQuorumUntil = ql.LatestRepliesReceived[gr.ReplicaId]
	}

	r.SendMsg(gr.ReplicaId, r.qleasePromiseRPC, p)
}

func (r *Replica) HandleQLeasePromise(ql *qlease.Lease, p *qleaseproto.Promise) bool {
	now := time.Now().UnixNano()
	// check that this promise was received on time
	if ql.LatestPromisesReceived[p.ReplicaId] < now && ql.GuardExpires[p.ReplicaId] < now {
		//didn't receive promise on time, must ignore
		//TODO: send NACK as optimization
		return false
	}

	if p.LeaseInstance < ql.PromisedToMeInst {
		// the sender must update its lease view
		pr := &qleaseproto.PromiseReply{r.Id, ql.PromisedToMeInst, p.TimestampNs}
		r.SendMsg(p.ReplicaId, r.qleasePromiseReplyRPC, pr)
		return false
	} else if p.LeaseInstance > ql.PromisedToMeInst {
		ql.PromisedToMeInst = p.LeaseInstance
		for i := int32(0); i < int32(r.N); i++ {
			ql.LatestPromisesReceived[i] = 0
		}
	}

	ql.LatestPromisesReceived[p.ReplicaId] = now + p.DurationNs

	//send reply
	pr := &qleaseproto.PromiseReply{r.Id, ql.PromisedToMeInst, p.TimestampNs}
	r.SendMsg(p.ReplicaId, r.qleasePromiseReplyRPC, pr)

	sorted := make([]int64, r.N)
	copy(sorted, ql.LatestPromisesReceived)
	sorted[r.Id] = 0
	sort.Sort(Int64Slice(sorted))

	ql.ReadLocallyUntil = sorted[r.N-(r.N/2)]

	return true
}

func (r *Replica) HandleQLeaseReply(ql *qlease.Lease, pr *qleaseproto.PromiseReply) {
	if pr.TimestampNs < ql.LatestTsSent {
		//old reply, ignore
		return
	}
	if pr.LeaseInstance > ql.PromisedByMeInst {
		ql.PromiseRejects++
		if ql.PromiseRejects == r.N {
			ql.WriteInQuorumUntil = 0
		}
		return
	}
	now := time.Now().UnixNano()
	max := now
	for i := int32(0); i < int32(r.N); i++ {
		if i == r.Id {
			continue
		}
		if i == pr.ReplicaId {
			ql.LatestRepliesReceived[i] = now + ql.Duration
		}
		if max < ql.LatestRepliesReceived[i] {
			max = ql.LatestRepliesReceived[i]
		}
	}

	ql.WriteInQuorumUntil = max

	r.LastReplyReceivedTimestamp[pr.ReplicaId] = now
}

// updates the preferred order in which to communicate with peers according to a preferred quorum
func (r *Replica) UpdatePreferredPeerOrder(quorum []int32) {
	aux := make([]int32, r.N)
	i := 0
	for _, p := range quorum {
		if p == r.Id {
			continue
		}
		aux[i] = p
		i++
	}

	for _, p := range r.PreferredPeerOrder {
		found := false
		for j := 0; j < i; j++ {
			if aux[j] == p {
				found = true
				break
			}
		}
		if !found {
			aux[i] = p
			i++
		}
	}

	r.PreferredPeerOrder = aux
}
