package consensus


import (
	"container/heap"
	"container/vector"
	"doozer/store"
	"goprotobuf.googlecode.com/hg/proto"
	"log"
	"sort"
	"time"
)


type packet struct {
	Addr string
	msg
}


func (p packet) Less(y interface{}) bool {
	return *p.Seqn < *y.(packet).Seqn
}


type Packet struct {
	Addr string
	Data []byte
}


type trigger struct {
	t int64 // trigger time
	n int64 // seqn
}


func (t trigger) Less(y interface{}) bool {
	return t.t < y.(trigger).t
}


type Stats struct {
	// Current queue sizes
	Runs        int
	WaitPackets int
	WaitTicks   int

	// Totals over all time
	TotalRuns  int64
	TotalFills int64
	TotalTicks int64
}


// DefRev is the rev in which this manager was defined;
// it will participate starting at DefRev+Alpha.
type Manager struct {
	Self   string
	DefRev int64
	Alpha  int64
	In     <-chan Packet
	Out    chan<- Packet
	Ops    chan<- store.Op
	PSeqn  chan<- int64
	Props  <-chan *Prop
	TFill  int64
	Store  *store.Store
	Ticker <-chan int64
	Stats  Stats
	run    map[int64]*run
	next   int64 // unused seqn
	fill   vector.Vector
	packet vector.Vector
	tick   vector.Vector
}


type Prop struct {
	Seqn int64
	Mut  []byte
}

var tickTemplate = &msg{Cmd: tick}
var fillTemplate = &msg{Cmd: propose, Value: []byte(store.Nop)}


func (m *Manager) Run() {
	m.run = make(map[int64]*run)
	runCh, err := m.Store.Wait(store.Any, m.DefRev)
	if err != nil {
		panic(err) // can't happen
	}

	for {
		m.Stats.Runs = len(m.run)
		m.Stats.WaitPackets = m.packet.Len()
		m.Stats.WaitTicks = m.tick.Len()

		select {
		case e, ok := <-runCh:
			if !ok {
				return
			}
			log.Println("event", e)

			runCh, err = m.Store.Wait(store.Any, e.Seqn+1)
			if err != nil {
				panic(err) // can't happen
			}

			m.event(e)
			m.Stats.TotalRuns++
			log.Println("runs:", fmtRuns(m.run))
			log.Println("avg tick delay:", avg(&m.tick))
			log.Println("avg fill delay:", avg(&m.fill))
		case p := <-m.In:
			recvPacket(&m.packet, p)
		case pr := <-m.Props:
			m.propose(&m.packet, pr, time.Nanoseconds())
		case t := <-m.Ticker:
			m.doTick(t)
		}

		m.pump()
	}
}


func (m *Manager) pump() {
	for m.packet.Len() > 0 {
		p := m.packet.At(0).(packet)
		log.Printf("p.seqn=%d m.next=%d", *p.Seqn, m.next)
		if *p.Seqn >= m.next {
			break
		}
		heap.Pop(&m.packet)

		r := m.run[*p.Seqn]
		if r == nil || r.l.done {
			go sendLearn(m.Out, p, m.Store)
		} else {
			r.update(p, &m.tick)
		}
	}
}


func (m *Manager) doTick(t int64) {
	n := applyTriggers(&m.packet, &m.fill, t, fillTemplate)
	m.Stats.TotalFills += int64(n)
	if n > 0 {
		log.Println("applied fills", n)
	}

	n = applyTriggers(&m.packet, &m.tick, t, tickTemplate)
	m.Stats.TotalTicks += int64(n)
	if n > 0 {
		log.Println("applied m.tick", n)
	}
}


func (m *Manager) propose(q heap.Interface, pr *Prop, t int64) {
	log.Println("prop", pr)
	msg := msg{Seqn: &pr.Seqn, Cmd: propose, Value: pr.Mut}
	heap.Push(q, packet{msg: msg})
	for n := pr.Seqn - 1; ; n-- {
		r := m.run[n]
		if r == nil || r.isLeader(m.Self) {
			break
		} else {
			schedTrigger(&m.fill, n, t, m.TFill)
		}
	}
}


func sendLearn(out chan<- Packet, p packet, st *store.Store) {
	if p.msg.Cmd != nil && *p.msg.Cmd == msg_INVITE {
		ch, err := st.Wait(store.Any, *p.Seqn)

		if err == store.ErrTooLate {
			log.Println(err)
		} else {
			e := <-ch
			m := msg{
				Seqn:  &e.Seqn,
				Cmd:   learn,
				Value: []byte(e.Mut),
			}
			buf, _ := proto.Marshal(&m)
			out <- Packet{p.Addr, buf}
		}
	}
}


func recvPacket(q heap.Interface, P Packet) {
	var p packet
	p.Addr = P.Addr

	err := proto.Unmarshal(P.Data, &p.msg)
	if err != nil {
		log.Println(err)
		return
	}

	if p.msg.Seqn == nil || p.msg.Cmd == nil {
		log.Printf("discarding %#v", p)
		return
	}

	log.Println("recv", p.Addr, *p.Seqn, msg_Cmd_name[int32(*p.Cmd)])
	heap.Push(q, p)
}


func avg(v *vector.Vector) (n int64) {
	t := time.Nanoseconds()
	if v.Len() == 0 {
		return -1
	}
	for _, x := range []interface{}(*v) {
		n += x.(trigger).t - t
	}
	return n / int64(v.Len())
}


func schedTrigger(q heap.Interface, n, t, tfill int64) {
	heap.Push(q, trigger{n: n, t: t + tfill})
}


func applyTriggers(packets, ticks *vector.Vector, now int64, tpl *msg) (n int) {
	for ticks.Len() > 0 {
		tt := ticks.At(0).(trigger)
		if tt.t > now {
			break
		}

		heap.Pop(ticks)

		p := packet{msg: *tpl}
		p.msg.Seqn = &tt.n
		log.Println("applying", *p.Seqn, msg_Cmd_name[int32(*p.Cmd)])
		heap.Push(packets, p)
		n++
	}
	return
}


func (m *Manager) event(e store.Event) {
	m.run[e.Seqn] = nil, false
	log.Printf("del run %d", e.Seqn)
	m.addRun(e)
}

func (m *Manager) addRun(e store.Event) (r *run) {
	r = new(run)
	r.self = m.Self
	r.out = m.Out
	r.ops = m.Ops
	r.bound = initialWaitBound
	r.seqn = e.Seqn + m.Alpha
	r.cals = getCals(e)
	r.addr = getAddrs(e, r.cals)
	if len(r.cals) < 1 {
		r.cals = m.run[r.seqn-1].cals
		r.addr = m.run[r.seqn-1].addr
	}
	r.c.size = len(r.cals)
	r.c.quor = r.quorum()
	r.c.crnd = r.indexOf(r.self) + int64(len(r.cals))
	r.l.init(int64(r.quorum()))
	m.run[r.seqn] = r
	if r.isLeader(m.Self) {
		log.Printf("pseqn %d", r.seqn)
		m.PSeqn <- r.seqn
	}
	log.Printf("add run %d", r.seqn)
	m.next = r.seqn + 1
	return r
}


func getCals(g store.Getter) []string {
	ents := store.Getdir(g, "/ctl/cal")
	cals := make([]string, len(ents))

	i := 0
	for _, cal := range ents {
		id := store.GetString(g, "/ctl/cal/"+cal)
		if id != "" {
			cals[i] = id
			i++
		}
	}

	cals = cals[0:i]
	sort.SortStrings(cals)

	return cals
}


func getAddrs(g store.Getter, cals []string) (a []string) {
	a = make([]string, len(cals))
	for i, id := range cals {
		a[i] = store.GetString(g, "/ctl/node/"+id+"/addr")
	}
	return
}


func fmtRuns(rs map[int64]*run) (s string) {
	var ns []int
	for i := range rs {
		ns = append(ns, int(i))
	}
	sort.SortInts(ns)
	for _, i := range ns {
		r := rs[int64(i)]
		if r.l.done {
			s += "X"
		} else if r.prop {
			s += "o"
		} else {
			s += "."
		}
	}
	return s
}
