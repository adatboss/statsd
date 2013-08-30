package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"log"
	"os"
	"sync"
	"time"
)

// TODO: remove debug info

const (
	fsDsPartitions = 4
	fsDsISize      = 16
	fsDsDSize      = 8
)

type FsDatastore struct {
	Dir, dir string
	gmu      sync.Mutex
	mu       [fsDsPartitions]sync.Mutex
	cond     [fsDsPartitions]sync.Cond
	notify   chan int
	streams  [fsDsPartitions]map[string]*fsDsStream
	queue    [fsDsPartitions][]*fsDsStream
	running  bool
}

type fsDsStream struct {
	sync.Mutex
	name     string
	dir      string
	tail     []fsDsRecord
	dat, idx *os.File
	valid    bool
	lastWr   int64
	dsize    int64
	isize    int64
}

type fsDsRecord struct {
	ts    int64
	value float64
}

type fsDsSnapshot struct {
	tail     []fsDsRecord
	dat, idx *os.File
	lastWr   int64
	dsize    int64
	isize    int64
}

func NewFsDatastore(dir string) *FsDatastore {
	return &FsDatastore{Dir: dir}
}

func (ds *FsDatastore) Open() error {
	ds.gmu.Lock()
	for p := 0; p < fsDsPartitions; p++ {
		ds.mu[p].Lock()
	}
	defer func() {
		for p := 0; p < fsDsPartitions; p++ {
			ds.mu[p].Unlock()
		}
		ds.gmu.Unlock()
	}()
	if ds.running {
		return Error("Datastore already running")
	}

	if fi, err := os.Stat(ds.Dir); err != nil {
		return err
	} else if !fi.IsDir() {
		return Error("Not a directory: " + ds.dir)
	}

	ds.dir = ds.Dir + string(os.PathSeparator)
	for p := 0; p < fsDsPartitions; p++ {
		ds.streams[p] = make(map[string]*fsDsStream)
		ds.cond[p].L = &ds.mu[p]
	}
	ds.notify = make(chan int)
	if err := ds.loadTails(); err != nil {
		return err
	}
	ds.running = true
	for p := 0; p < fsDsPartitions; p++ {
		go ds.write(ds.notify, p)
	}
	return nil
}

func (ds *FsDatastore) Close() error {
	ds.gmu.Lock()
	if !ds.running {
		ds.gmu.Unlock()
		return Error("Datastore not running")
	}
	for p := 0; p < fsDsPartitions; p++ {
		ds.mu[p].Lock()
	}
	if err := ds.saveTails(); err != nil {
		log.Println("FsDatastore.Close:", err)
		if err := os.Remove(ds.dir + "tail_data"); err != nil {
			log.Println("FsDatastore.Close:", err)
		}
	}
	notify := ds.notify
	ds.running = false
	for p := 0; p < fsDsPartitions; p++ {
		ds.streams[p] = nil
		ds.queue[p] = nil
		ds.cond[p].Signal()
		ds.mu[p].Unlock()
		<-notify
	}
	ds.gmu.Unlock()
	return nil
}

func (ds *FsDatastore) Insert(name string, r Record) error {
	log.Println("inserting:", name)
	st := ds.getStream(name)
	if st == nil {
		return Error("Datastore not running")
	}
	st.tail = append(st.tail, fsDsRecord{ts: r.Ts, value: r.Value})
	st.Unlock()
	return nil
}

func (ds *FsDatastore) Query(name string, from, until int64) ([]Record, error) {
	s, err := ds.makeSnapshot(name)
	if s != nil {
		return []Record{}, err
	}
	defer s.close()

	// TODO
	_ = s

	return []Record{}, nil
}

func (ds *FsDatastore) LatestBefore(name string, ts int64) (Record, error) {
	s, err := ds.makeSnapshot(name)
	if err != nil {
		return Record{}, err
	}
	defer s.close()

	ts -= ts % 60

	log.Println("latest:", name, ts)
	if n := s.findTail(ts); n != -1 {
		log.Println("latest:", name, "found in tail")
		return Record{Ts: s.tail[n].ts, Value: s.tail[n].value}, nil
	}

	n, err := s.findIdx(ts)
	if err != nil {
		return Record{}, err
	}
	if n == -1 {
		log.Println("latest:", name, "not found on disk")
		return Record{}, ErrNoData
	}

	log.Println("latest:", name, "found on disk:", n)

	t, pos, err := s.readIdxEntry(n)
	if err != nil {
		return Record{}, err
	}
	log.Println("latest:", name, t, pos)

	var lastPos int64
	if n == s.isize/fsDsISize-1 {
		lastPos = s.dsize - fsDsDSize
	} else {
		_, p, err := s.readIdxEntry(n + 1)
		if err != nil {
			return Record{}, err
		}
		lastPos = p - fsDsDSize
	}

	if _, err := s.dat.Seek(lastPos, os.SEEK_SET); err != nil {
		return Record{}, err
	}
	var val float64
	if err := binary.Read(s.dat, binary.LittleEndian, &val); err != nil {
		return Record{}, err
	}
	return Record{Ts: t + 60*((lastPos-pos)/fsDsDSize), Value: val}, nil
}

func (ds *FsDatastore) getStream(name string) *fsDsStream {
	p := ds.partition(name)
	ds.mu[p].Lock()
	if !ds.running {
		ds.mu[p].Unlock()
		return nil
	}
	if _, ok := ds.streams[p][name]; !ok {
		ds.createStream(name, p, nil)
	}
	st := ds.streams[p][name]
	st.Lock()
	ds.mu[p].Unlock()
	return st
}

func (ds *FsDatastore) makeSnapshot(name string) (*fsDsSnapshot, error) {
	st := ds.getStream(name)
	if st == nil {
		return nil, Error("Datastore not running")
	}
	s, err := st.makeSnapshot()
	if err != nil {
		st.Unlock()
		return nil, err
	}
	st.Unlock()
	return s, nil
}

func (ds *FsDatastore) createStream(name string, p uint, tail []fsDsRecord) {
	st := &fsDsStream{
		name: name,
		dir:  ds.dir,
		tail: tail,
	}
	ds.streams[p][name] = st
	ds.queue[p] = append(ds.queue[p], st)
	if len(ds.queue[p]) == 1 {
		ds.cond[p].Signal()
	}
	log.Println("loaded: ", name)
}

func (ds *FsDatastore) write(notify chan int, p int) {
	for n := -1; ; {
		ds.mu[p].Lock()
		if len(ds.queue[p]) == 0 && ds.running {
			ds.cond[p].Wait()
		}
		if !ds.running {
			ds.mu[p].Unlock()
			notify <- 1
			return
		}
		l := len(ds.queue[p])
		if n++; n >= l {
			n = 0
		}
		st := ds.queue[p][n]
		st.Lock()
		if len(st.tail) == 0 {
			ds.queue[p][n] = ds.queue[p][l-1]
			ds.queue[p][l-1] = nil
			ds.queue[p] = ds.queue[p][0 : l-1]
			delete(ds.streams[p], st.name)
			if cap(ds.queue[p]) > 3*(l-1) {
				log.Println("queue shrink:", cap(ds.queue[p]), l-1)
				x := make([]*fsDsStream, l-1, 2*(l-1))
				copy(x, ds.queue[p])
				ds.queue[p] = x
			}
			st.Unlock()
			ds.mu[p].Unlock()
			log.Println("delete:", st.name)
		} else {
			ds.mu[p].Unlock()
			if err := st.writeTail(); err != nil {
				st.valid = false
				log.Println("write:", err)
			}
			if cap(st.tail) > 3*len(st.tail) {
				log.Println("tail shrink:", cap(st.tail), len(st.tail))
				st.tail = make([]fsDsRecord, 0, 2*len(st.tail))
			} else {
				st.tail = st.tail[:0]
			}
			st.Unlock()
		}
	}
}

func (ds *FsDatastore) saveTails() error {
	log.Println("saveTailes()...")
	start := time.Now()

	f, err := os.Create(ds.dir + "tail_data")
	if err != nil {
		return err
	}
	defer f.Close()
	wr, le := bufio.NewWriter(f), binary.LittleEndian

	ntails := 0
	for _, streams := range ds.streams {
		ntails += len(streams)
	}
	if err = binary.Write(wr, le, uint64(ntails)); err != nil {
		return err
	}

	var (
		n  string
		st *fsDsStream
	)
	i := 0
	for _, streams := range ds.streams {
		for n, st = range streams {
			i++
			st.Lock()
			name := []byte(n)
			if err = binary.Write(wr, le, uint64(len(name))); err != nil {
				break
			}
			if err = binary.Write(wr, le, uint64(len(st.tail))); err != nil {
				break
			}
			if err = binary.Write(wr, le, name); err != nil {
				break
			}
			if err = binary.Write(wr, le, st.tail); err != nil {
				break
			}
			st.Unlock()
			log.Println("tail saved:", i)
		}
	}
	if err != nil {
		st.Unlock()
		return err
	}

	if err = wr.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}

	finish := time.Now()
	log.Println("done.", finish.Sub(start).Seconds(), i)

	return nil
}

func (ds *FsDatastore) loadTails() error {
	log.Println("loadTails()...")
	start := time.Now()

	f, err := os.Open(ds.dir + "tail_data")
	if os.IsNotExist(err) {
		log.Println("done.")
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	rd, le := bufio.NewReader(f), binary.LittleEndian

	var ntails int64
	if err = binary.Read(rd, le, &ntails); err != nil {
		return err
	}

	for i := int64(0); i < ntails; i++ {
		var lname, ltail int64
		if err = binary.Read(rd, le, &lname); err != nil {
			return err
		}
		if err = binary.Read(rd, le, &ltail); err != nil {
			return err
		}
		name := make([]byte, lname)
		if err = binary.Read(rd, le, &name); err != nil {
			return err
		}
		tail := make([]fsDsRecord, ltail)
		if err = binary.Read(rd, le, &tail); err != nil {
			return err
		}
		strName := string(name)
		ds.createStream(strName, ds.partition(strName), tail)
	}

	finish := time.Now()
	log.Println("done.", finish.Sub(start).Seconds())

	return nil
}

func (st *fsDsStream) writeTail() error {
	log.Println(st.dir+st.name, len(st.tail))
	if err := st.openFiles(); err != nil {
		return err
	}
	defer st.closeFiles()

	dbuff, ibuff := new(bytes.Buffer), new(bytes.Buffer)
	dsize, isize, lastWr := st.dsize, st.isize, st.lastWr

	for _, r := range st.tail {
		if r.ts%60 != 0 {
			log.Println("fsDsStream.writeTail: Timestamp not divisible by 60")
			continue
		} else if lastWr >= r.ts {
			log.Println("fsDsStream.writeTail: Timestamp in the past")
			continue
		}

		le := binary.LittleEndian
		binary.Write(dbuff, le, r.value)
		dsize += fsDsDSize
		lastWr += 60

		if r.ts > lastWr {
			binary.Write(ibuff, le, []int64{r.ts, dsize - fsDsDSize})
			isize += fsDsISize
			lastWr = r.ts
		}
	}

	if _, err := st.dat.Seek(0, os.SEEK_END); err != nil {
		return err
	}
	if _, err := st.idx.Seek(0, os.SEEK_END); err != nil {
		return err
	}

	if _, err := dbuff.WriteTo(st.dat); err != nil {
		return err
	}
	if _, err := ibuff.WriteTo(st.idx); err != nil {
		return err
	}

	st.dsize, st.isize, st.lastWr = dsize, isize, lastWr
	return nil
}

func (st *fsDsStream) openFiles() error {
	dat, err := os.OpenFile(st.dir+st.name+".dat", os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	idx, err := os.OpenFile(st.dir+st.name+".idx", os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		dat.Close()
		return err
	}
	st.dat, st.idx = dat, idx

	if !st.valid {
		di, err := dat.Stat()
		if err != nil {
			st.closeFiles()
			return err
		}
		ii, err := idx.Stat()
		if err != nil {
			st.closeFiles()
			return err
		}
		st.dsize, st.isize = di.Size(), ii.Size()
		if st.isize%fsDsISize != 0 || st.dsize%fsDsDSize != 0 {
			st.closeFiles()
			return Error("Invalid file size: " + st.name)
		}

		if st.isize == 0 {
			st.lastWr = -1<<63 - (-1<<63)%60
		} else {
			if _, err := st.idx.Seek(st.isize-fsDsISize, os.SEEK_SET); err != nil {
				st.closeFiles()
				return err
			}
			d := []int64{0, 0}
			if err := binary.Read(st.idx, binary.LittleEndian, d); err != nil {
				st.closeFiles()
				return err
			}
			ts, pos := d[0], d[1]
			st.lastWr = ts + 60*((st.dsize-pos)/fsDsDSize-1)
		}
		st.valid = true
	}

	return nil
}

func (st *fsDsStream) closeFiles() {
	if st.dat != nil {
		if err := st.dat.Sync(); err != nil {
			log.Println("fsDsStream.closeFiles:", err)
		}
		st.dat.Close()
		st.dat = nil
	}
	if st.idx != nil {
		if err := st.idx.Sync(); err != nil {
			log.Println("fsDsStream.closeFiles:", err)
		}
		st.idx.Close()
		st.idx = nil
	}
}

func (st *fsDsStream) makeSnapshot() (*fsDsSnapshot, error) {
	if err := st.openFiles(); err != nil {
		return nil, err
	}
	s := &fsDsSnapshot{
		tail:   append([]fsDsRecord(nil), st.tail...),
		dat:    st.dat,
		idx:    st.idx,
		lastWr: st.lastWr,
		dsize:  st.dsize,
		isize:  st.isize,
	}
	st.dat, st.dat = nil, nil
	return s, nil
}

func (s *fsDsSnapshot) close() {
	s.dat.Close()
	s.idx.Close()
	s.dat, s.idx = nil, nil
}

func (s *fsDsSnapshot) findIdx(ts int64) (int64, error) {
	if s.isize == 0 {
		log.Println("findIdx: isize == 0")
		return -1, nil
	}

	first, _, err := s.readIdxEntry(0)
	if err != nil {
		return 0, err
	}
	if first > ts {
		return -1, nil
	}

	i, j := int64(0), s.isize/fsDsISize-1
	for i < j {
		log.Println("findIdx:", i, j)
		k := (i + j) / 2
		t, _, err := s.readIdxEntry(k)
		if err != nil {
			return 0, err
		}
		switch {
		case t == ts:
			i, j = k, k
		case t > ts:
			j = k - 1
		case t < ts:
			if i != k {
				i = k
			} else {
				// j == i+1
				x, _, err := s.readIdxEntry(j)
				if err != nil {
					return 0, err
				}
				if x > ts {
					j = i
				} else {
					i = j
				}
			}
		}
	}
	return i, nil
}

func (s *fsDsSnapshot) findTail(ts int64) int64 {
	last, k := s.lastWr, -1
	for i, r := range s.tail {
		if r.ts%60 != 0 || last >= r.ts {
			continue
		}
		if r.ts <= ts {
			k = i
		} else {
			break
		}
		last = r.ts
	}
	return int64(k)
}

func (s *fsDsSnapshot) readIdxEntry(n int64) (ts int64, pos int64, err error) {
	if _, err := s.idx.Seek(n*fsDsISize, os.SEEK_SET); err != nil {
		return 0, 0, err
	}
	d := [2]int64{}
	if err := binary.Read(s.idx, binary.LittleEndian, d[:]); err != nil {
		return 0, 0, err
	}
	if d[0]%60 != 0 || d[1]%fsDsDSize != 0 {
		return 0, 0, Error("Invalid index data")
	}
	return d[0], d[1], nil
}

func (ds *FsDatastore) partition(name string) uint {
	var x uint64
	for _, ch := range name {
		for i := 15; i >= 0; i-- {
			x <<= 1
			x ^= 0x1edc6f41 * ((x >> 32) ^ (uint64(ch)>>uint(i))&1)
		}
	}
	return uint((x & 0xffff) % fsDsPartitions)
}
