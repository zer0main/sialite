package cache

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"path"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/types"
	"github.com/golang/snappy"
	"github.com/starius/sialite/emsort"
	"github.com/starius/sialite/fastmap"
)

type parameters struct {
	OffsetLen               int
	OffsetIndexLen          int
	AddressPageLen          int
	AddressPrefixLen        int
	AddressFastmapPrefixLen int
	AddressOffsetLen        int
}

type blockHeader struct {
	Nonce      types.BlockNonce
	Timestamp  types.Timestamp
	MerkleRoot crypto.Hash
}

type Builder struct {
	blockchain      *os.File
	blockchainBuf   *bufio.Writer
	blockchainLen   uint64
	dataBuf         bytes.Buffer
	compressedBuf   []byte
	leavesHashes    *os.File
	leavesHashesBuf *bufio.Writer

	siaHash    hash.Hash
	siaHashBuf []byte

	// Series of blockHeader.
	headersFile    *os.File
	headersEncoder *encoding.Encoder

	offsetIndex uint64

	// 8-byte offsets of miner payouts, and txs in blockchain
	offsets *os.File

	// list of pairs (index of first miner payout, index of first tx) in offsets
	// Indices are offsetLen byte long
	blockLocations *os.File

	// unlockhash(addressPrefixLen bytes) + addressOffsetLen byte index in offsets
	addresses    emsort.SortedWriter
	addressestmp *os.File

	buf, tmpBuf []byte

	offsetEnd uint64

	offsetLen, offsetIndexLen           int
	addressRecordSize, addressPrefixLen int
}

func NewBuilder(dir string, memLimit, offsetLen, offsetIndexLen, addressPageLen, addressPrefixLen, addressFastmapPrefixLen, addressOffsetLen int) (*Builder, error) {

	bufferSize := 8 // Max of used buffers.
	addressRecordSize := addressPrefixLen + offsetIndexLen
	if addressRecordSize > bufferSize {
		bufferSize = addressRecordSize
	}

	if list, err := ioutil.ReadDir(dir); err != nil {
		return nil, fmt.Errorf("ioutil.ReadDir(%q): %v", dir, err)
	} else if len(list) != 0 {
		return nil, fmt.Errorf("Output directory is not empty")
	}

	p := parameters{
		OffsetLen:               offsetLen,
		OffsetIndexLen:          offsetIndexLen,
		AddressPageLen:          addressPageLen,
		AddressPrefixLen:        addressPrefixLen,
		AddressFastmapPrefixLen: addressFastmapPrefixLen,
		AddressOffsetLen:        addressOffsetLen,
	}

	parametersJson, err := os.Create(path.Join(dir, "parameters.json"))
	if err != nil {
		return nil, fmt.Errorf("opening parameters.json: %v", err)
	}
	e := json.NewEncoder(parametersJson)
	e.SetIndent("", "\t")
	if err := e.Encode(p); err != nil {
		return nil, fmt.Errorf("JSON Encode: %v", err)
	}
	if err := parametersJson.Close(); err != nil {
		return nil, fmt.Errorf("JSON Close: %v", err)
	}

	blockchain, err := os.Create(path.Join(dir, "blockchain"))
	if err != nil {
		return nil, fmt.Errorf("opening blockchain: %v", err)
	}

	leavesHashes, err := os.Create(path.Join(dir, "leavesHashes"))
	if err != nil {
		return nil, fmt.Errorf("opening leavesHashes: %v", err)
	}

	headersFile, err := os.Create(path.Join(dir, "headers"))
	if err != nil {
		return nil, fmt.Errorf("opening headers: %v", err)
	}
	headersEncoder := encoding.NewEncoder(headersFile)

	offsets, err := os.Create(path.Join(dir, "offsets"))
	if err != nil {
		return nil, fmt.Errorf("opening offsets: %v", err)
	}

	blockLocations, err := os.Create(path.Join(dir, "blockLocations"))
	if err != nil {
		return nil, fmt.Errorf("opening blockLocations: %v", err)
	}

	addressesFastmapData, err := os.Create(path.Join(dir, "addressesFastmapData"))
	if err != nil {
		return nil, fmt.Errorf("opening addressesFastmapData: %v", err)
	}
	addressesFastmapPrefixes, err := os.Create(path.Join(dir, "addressesFastmapPrefixes"))
	if err != nil {
		return nil, fmt.Errorf("opening addressesFastmapPrefixes: %v", err)
	}

	addressesIndices, err := os.Create(path.Join(dir, "addressesIndices"))
	if err != nil {
		return nil, fmt.Errorf("opening addressesIndices: %v", err)
	}

	var inliner fastmap.Inliner = fastmap.NoInliner{}
	containerLen := offsetIndexLen
	if addressOffsetLen == offsetIndexLen {
		inliner = fastmap.NewFFOOInliner(offsetIndexLen)
		containerLen = 2 * offsetIndexLen
	}

	addressesMultiMapWriter, err := fastmap.NewMultiMapWriter(addressPageLen, addressPrefixLen, offsetIndexLen, addressFastmapPrefixLen, addressOffsetLen, containerLen, addressesFastmapData, addressesFastmapPrefixes, addressesIndices, inliner)
	if err != nil {
		return nil, fmt.Errorf("fastmap.NewMultiMapWriter: %v", err)
	}

	addressestmp, err := os.Create(path.Join(dir, "addresses.tmp"))
	if err != nil {
		return nil, fmt.Errorf("opening addresses.tmp: %v", err)
	}
	addresses, err := emsort.New(addressesMultiMapWriter, addressRecordSize, emsort.BytesLess, memLimit, addressestmp)
	if err != nil {
		return nil, fmt.Errorf("emsort.New: %v", err)
	}

	if offsetLen > 8 {
		return nil, fmt.Errorf("too large offsetLen")
	}

	return &Builder{
		blockchain:      blockchain,
		blockchainBuf:   bufio.NewWriter(blockchain),
		leavesHashes:    leavesHashes,
		leavesHashesBuf: bufio.NewWriter(leavesHashes),
		siaHash:         crypto.NewHash(),

		headersFile:    headersFile,
		headersEncoder: headersEncoder,

		offsets:        offsets,
		blockLocations: blockLocations,
		addresses:      addresses,

		addressestmp: addressestmp,

		buf:    make([]byte, bufferSize),
		tmpBuf: make([]byte, 8),

		offsetEnd: uint64((1 << uint(8*offsetLen)) - 1),

		offsetLen:         offsetLen,
		offsetIndexLen:    offsetIndexLen,
		addressRecordSize: addressRecordSize,
		addressPrefixLen:  addressPrefixLen,
	}, nil
}

func (s *Builder) Add(block *types.Block) error {
	header := blockHeader{
		Nonce:      block.Nonce,
		Timestamp:  block.Timestamp,
		MerkleRoot: block.MerkleRoot(),
	}
	if err := s.headersEncoder.Encode(header); err != nil {
		return err
	}
	offsetFull := s.buf[:8]
	offset := s.buf[:s.offsetLen]
	blockLoc := s.buf[:s.offsetIndexLen*2]
	addressLoc := s.buf[:s.addressRecordSize]
	addressPrefix := addressLoc[:s.addressPrefixLen]
	locOfAddress := addressLoc[s.addressPrefixLen:s.addressRecordSize]
	writeAddress := func(uh types.UnlockHash) error {
		copy(addressPrefix, uh[:])
		if n, err := s.addresses.Write(addressLoc); err != nil {
			return err
		} else if n != s.addressRecordSize {
			return io.ErrShortWrite
		}
		return nil
	}
	firstMinerPayout := s.offsetIndex
	// See Block.MarshalSia.
	for _, mp := range block.MinerPayouts {
		binary.LittleEndian.PutUint64(offsetFull, s.blockchainLen)
		if n, err := s.offsets.Write(offset); err != nil {
			return err
		} else if n != s.offsetLen {
			return io.ErrShortWrite
		}
		wireOffsetIndex := s.offsetIndex + 1 // To avoid special 0 value on wire.
		binary.LittleEndian.PutUint64(s.tmpBuf, wireOffsetIndex)
		copy(locOfAddress, s.tmpBuf)
		if err := writeAddress(mp.UnlockHash); err != nil {
			return err
		}
		s.offsetIndex++
		if err := mp.MarshalSia(&s.dataBuf); err != nil {
			return err
		}
		s.siaHash.Reset()
		_, _ = s.siaHash.Write([]byte{0x00})
		_, _ = s.siaHash.Write(s.dataBuf.Bytes())
		s.siaHashBuf = s.siaHash.Sum(s.siaHashBuf[:0])
		if _, err := s.leavesHashesBuf.Write(s.siaHashBuf); err != nil {
			return err
		}
		s.blockchainLen += uint64(s.dataBuf.Len())
		if _, err := s.dataBuf.WriteTo(s.blockchainBuf); err != nil {
			return err
		}
	}
	firstTransaction := s.offsetIndex
	for i, tx := range block.Transactions {
		binary.LittleEndian.PutUint64(offsetFull, s.blockchainLen)
		if n, err := s.offsets.Write(offset); err != nil {
			return err
		} else if n != s.offsetLen {
			return io.ErrShortWrite
		}
		wireOffsetIndex := s.offsetIndex + 1 // To avoid special 0 value on wire.
		binary.LittleEndian.PutUint64(s.tmpBuf, wireOffsetIndex)
		copy(locOfAddress, s.tmpBuf)
		for _, si := range tx.SiacoinInputs {
			if err := writeAddress(si.UnlockConditions.UnlockHash()); err != nil {
				return err
			}
		}
		for _, si := range tx.SiafundInputs {
			if err := writeAddress(si.UnlockConditions.UnlockHash()); err != nil {
				return err
			}
		}
		for _, so := range tx.SiacoinOutputs {
			if err := writeAddress(so.UnlockHash); err != nil {
				return err
			}
		}
		for _, so := range tx.SiafundOutputs {
			if err := writeAddress(so.UnlockHash); err != nil {
				return err
			}
		}
		for _, contract := range tx.FileContracts {
			for _, so := range contract.ValidProofOutputs {
				if err := writeAddress(so.UnlockHash); err != nil {
					return err
				}
			}
			for _, so := range contract.MissedProofOutputs {
				if err := writeAddress(so.UnlockHash); err != nil {
					return err
				}
			}
		}
		for _, rev := range tx.FileContractRevisions {
			for _, so := range rev.NewValidProofOutputs {
				if err := writeAddress(so.UnlockHash); err != nil {
					return err
				}
			}
			for _, so := range rev.NewMissedProofOutputs {
				if err := writeAddress(so.UnlockHash); err != nil {
					return err
				}
			}
		}
		s.offsetIndex++
		if err := block.Transactions[i].MarshalSia(&s.dataBuf); err != nil {
			return err
		}
		s.siaHash.Reset()
		_, _ = s.siaHash.Write([]byte{0x00})
		_, _ = s.siaHash.Write(s.dataBuf.Bytes())
		s.siaHashBuf = s.siaHash.Sum(s.siaHashBuf[:0])
		if _, err := s.leavesHashesBuf.Write(s.siaHashBuf); err != nil {
			return err
		}
		s.compressedBuf = snappy.Encode(s.compressedBuf, s.dataBuf.Bytes())
		s.dataBuf.Reset()
		s.blockchainLen += uint64(len(s.compressedBuf))
		if _, err := s.blockchainBuf.Write(s.compressedBuf); err != nil {
			return err
		}
	}
	binary.LittleEndian.PutUint64(s.tmpBuf, firstMinerPayout)
	copy(blockLoc[:s.offsetIndexLen], s.tmpBuf)
	binary.LittleEndian.PutUint64(s.tmpBuf, firstTransaction)
	copy(blockLoc[s.offsetIndexLen:], s.tmpBuf)
	if n, err := s.blockLocations.Write(blockLoc); err != nil {
		return err
	} else if n != len(blockLoc) {
		return io.ErrShortWrite
	}
	if s.blockchainLen > s.offsetEnd {
		return fmt.Errorf("too large offset (%d > %d); increase offsetLen", s.blockchainLen, s.offsetEnd)
	}
	return nil
}

func (s *Builder) Close() error {
	if err := s.blockchainBuf.Flush(); err != nil {
		return err
	}
	if err := s.blockchain.Close(); err != nil {
		return err
	}
	if err := s.leavesHashesBuf.Flush(); err != nil {
		return err
	}
	if err := s.leavesHashes.Close(); err != nil {
		return err
	}
	if err := s.headersFile.Close(); err != nil {
		return err
	}
	if err := s.offsets.Close(); err != nil {
		return err
	}
	if err := s.blockLocations.Close(); err != nil {
		return err
	}
	if err := s.addresses.Close(); err != nil {
		return err
	}
	if err := s.addressestmp.Close(); err != nil {
		return err
	}
	if err := os.Remove(s.addressestmp.Name()); err != nil {
		return err
	}
	return nil
}
