package cache

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/merkletree"
	"github.com/starius/sialite/fastmap"
)

const (
	MAX_HISTORY_SIZE = 2
)

type Server struct {
	Blockchain     []byte
	Offsets        []byte
	BlockLocations []byte
	LeavesHashes   []byte

	AddressesFastmapData     []byte
	AddressesFastmapPrefixes []byte
	AddressesIndices         []byte
	addressMap               *fastmap.MultiMap

	offsetLen        int
	offsetIndexLen   int
	addressPrefixLen int

	nblocks, nitems int
}

func NewServer(dir string) (*Server, error) {
	// Read parameters.json.
	jf, err := os.Open(path.Join(dir, "parameters.json"))
	if err != nil {
		return nil, err
	}
	defer jf.Close()
	var par parameters
	if err := json.NewDecoder(jf).Decode(&par); err != nil {
		return nil, err
	}
	s := &Server{
		offsetLen:        par.OffsetLen,
		offsetIndexLen:   par.OffsetIndexLen,
		addressPrefixLen: par.AddressPrefixLen,
	}
	v := reflect.ValueOf(s).Elem()
	st := v.Type()
	// Mmap all []byte fileds from files.
	for i := 0; i < st.NumField(); i++ {
		ft := st.Field(i)
		if ft.Type == reflect.TypeOf([]byte{}) {
			name := strings.ToLower(ft.Name[:1]) + ft.Name[1:]
			f, err := os.Open(path.Join(dir, name))
			if err != nil {
				return nil, err
			}
			defer f.Close()
			stat, err := f.Stat()
			if err != nil {
				return nil, err
			}
			buf, err := syscall.Mmap(int(f.Fd()), 0, int(stat.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				return nil, err
			}
			v.Field(i).SetBytes(buf)
		}
	}
	var uninliner fastmap.Uninliner = fastmap.NoUninliner{}
	containerLen := par.OffsetIndexLen
	if par.AddressOffsetLen == par.OffsetIndexLen {
		uninliner = fastmap.NewFFOOInliner(par.OffsetIndexLen)
		containerLen = 2 * par.OffsetIndexLen
	}
	addressMap, err := fastmap.OpenMultiMap(par.AddressPageLen, par.AddressPrefixLen, par.OffsetIndexLen, par.AddressOffsetLen, containerLen, s.AddressesFastmapData, s.AddressesFastmapPrefixes, s.AddressesIndices, uninliner)
	if err != nil {
		return nil, err
	}
	s.addressMap = addressMap
	s.nblocks = len(s.BlockLocations) / (2 * par.OffsetIndexLen)
	if s.nblocks*(2*par.OffsetIndexLen) != len(s.BlockLocations) {
		return nil, fmt.Errorf("Bad length of blockLocations")
	}
	s.nitems = len(s.Offsets) / par.OffsetLen
	if s.nitems*par.OffsetLen != len(s.Offsets) {
		return nil, fmt.Errorf("Bad length of offsets")
	}
	runtime.SetFinalizer(s, (*Server).Close)
	return s, nil
}

func (s *Server) Close() error {
	v := reflect.ValueOf(s).Elem()
	st := v.Type()
	for i := 0; i < st.NumField(); i++ {
		ft := st.Field(i)
		if ft.Type == reflect.TypeOf([]byte{}) {
			buf := v.Field(i).Interface().([]byte)
			if err := syscall.Munmap(buf); err != nil {
				return err
			}
		}
	}
	return nil
}

const (
	MINER_PAYOUT = 0
	TRANSACTION  = 1
)

const (
	NO_COMPRESSION = 0
	SNAPPY         = 1
)

type Item struct {
	Data            []byte
	Compression     int
	Block           int
	Index           int
	NumLeaves       int
	NumMinerPayouts int
	MerkleProof     []byte
}

func (s *Server) GetHistory(address []byte, start string) (history []Item, next string, err error) {
	if len(address) != crypto.HashSize {
		return nil, "", fmt.Errorf("size of address: want %d, got %d", crypto.HashSize, len(address))
	}
	addressPrefix := address[:s.addressPrefixLen]
	values, err := s.addressMap.Lookup(addressPrefix)
	if err != nil || values == nil {
		return nil, "", err
	}
	size := len(values) / s.offsetIndexLen
	if size > MAX_HISTORY_SIZE {
		size = MAX_HISTORY_SIZE
		// TODO implement "next" logic.
	}
	indexPos := 0
	var tmp [8]byte
	tmpBytes := tmp[:]
	for i := 0; i < size; i++ {
		indexEnd := indexPos + s.offsetIndexLen
		copy(tmpBytes, values[indexPos:indexEnd])
		// Value 0 is special on wire, so all indices are shifted.
		wireItemIndex := int(binary.LittleEndian.Uint64(tmpBytes))
		itemIndex := wireItemIndex - 1
		indexPos = indexEnd
		item, err := s.GetItem(itemIndex)
		if err != nil {
			return nil, "", err
		}
		history = append(history, item)
	}
	return history, "", nil
}

var (
	ErrTooLargeIndex = fmt.Errorf("Error in database: too large item index")
)

func (s *Server) GetItem(itemIndex int) (Item, error) {
	var tmp [8]byte
	tmpBytes := tmp[:]
	if itemIndex >= s.nitems {
		return Item{}, ErrTooLargeIndex
	}
	start := itemIndex * s.offsetLen
	copy(tmpBytes, s.Offsets[start:start+s.offsetLen])
	dataStart := int(binary.LittleEndian.Uint64(tmpBytes))
	dataEnd := len(s.Blockchain)
	if itemIndex != s.nitems-1 {
		copy(tmpBytes, s.Offsets[start+s.offsetLen:start+2*s.offsetLen])
		dataEnd = int(binary.LittleEndian.Uint64(tmpBytes))
	}
	data := s.Blockchain[dataStart:dataEnd]
	// Find the block.
	blockIndex := sort.Search(s.nblocks, func(i int) bool {
		payoutsStart := s.getPayoutsStart(i)
		return payoutsStart > itemIndex
	}) - 1
	payoutsStart, txsStart, nleaves := s.getBlockLocation(blockIndex)
	item := Item{
		Data:      data,
		Block:     blockIndex,
		NumLeaves: nleaves,
		Index:     itemIndex - payoutsStart,
	}
	if itemIndex < txsStart {
		item.Compression = NO_COMPRESSION
	} else {
		item.Compression = SNAPPY
	}
	// Build MerkleProof.
	hstart := payoutsStart * crypto.HashSize
	hstop := hstart + nleaves*crypto.HashSize
	leavesHashes := s.LeavesHashes[hstart:hstop]
	tree := merkletree.NewCachedTree(crypto.NewHash(), 0)
	if err := tree.SetIndex(uint64(item.Index)); err != nil {
		return Item{}, fmt.Errorf("tree.SetIndex(%d): %v", item.Index, err)
	}
	for i := 0; i < nleaves; i++ {
		start := i * crypto.HashSize
		stop := start + crypto.HashSize
		tree.Push(leavesHashes[start:stop])
	}
	_, proofSet, _, _ := tree.Prove(nil)
	proof := make([]byte, 0, len(proofSet)*crypto.HashSize)
	for _, h := range proofSet {
		if len(h) != crypto.HashSize {
			panic("len(h)=" + string(len(h)))
		}
		proof = append(proof, h...)
	}
	item.MerkleProof = proof
	return item, nil
}

func (s *Server) getBlockLocation(index int) (int, int, int) {
	var tmp [8]byte
	tmpBytes := tmp[:]
	p1 := index * (2 * s.offsetIndexLen)
	p2 := p1 + s.offsetIndexLen
	p3 := p2 + s.offsetIndexLen
	p4 := p3 + s.offsetIndexLen
	copy(tmpBytes, s.BlockLocations[p1:p2])
	payoutsStart := int(binary.LittleEndian.Uint64(tmpBytes))
	copy(tmpBytes, s.BlockLocations[p2:p3])
	txsStart := int(binary.LittleEndian.Uint64(tmpBytes))
	nextStart := s.nitems
	if index != s.nblocks-1 {
		copy(tmpBytes, s.BlockLocations[p3:p4])
		nextStart = int(binary.LittleEndian.Uint64(tmpBytes))
	}
	nleaves := nextStart - payoutsStart
	return payoutsStart, txsStart, nleaves
}

func (s *Server) getPayoutsStart(index int) int {
	var tmp [8]byte
	tmpBytes := tmp[:]
	p1 := index * (2 * s.offsetIndexLen)
	p2 := p1 + s.offsetIndexLen
	copy(tmpBytes, s.BlockLocations[p1:p2])
	payoutsStart := int(binary.LittleEndian.Uint64(tmpBytes))
	return payoutsStart
}
