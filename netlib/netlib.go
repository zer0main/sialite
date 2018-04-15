package netlib

import (
	"fmt"
	"io"
	"log"
	"net"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/modules/consensus"
	"github.com/NebulousLabs/Sia/types"
)

type sessionHeader struct {
	GenesisID  types.BlockID
	UniqueID   [8]byte
	NetAddress modules.NetAddress
}

func Connect(node string) (net.Conn, error) {
	log.Println("Using node: ", node)
	conn, err := net.Dial("tcp", node)
	if err != nil {
		return nil, err
	}
	version := build.Version
	if err := encoding.WriteObject(conn, version); err != nil {
		return nil, err
	}
	if err := encoding.ReadObject(conn, &version, uint64(100)); err != nil {
		return nil, err
	}
	log.Println(version)
	sh := sessionHeader{
		GenesisID:  types.GenesisID,
		UniqueID:   [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		NetAddress: modules.NetAddress("example.com:1111"),
	}
	if err := encoding.WriteObject(conn, sh); err != nil {
		return nil, err
	}
	var response string
	if err := encoding.ReadObject(conn, &response, 100); err != nil {
		return nil, fmt.Errorf("failed to read header acceptance: %v", err)
	} else if response == modules.StopResponse {
		return nil, fmt.Errorf("peer did not want a connection")
	} else if response != modules.AcceptResponse {
		return nil, fmt.Errorf("peer rejected our header: %v", response)
	}
	if err := encoding.ReadObject(conn, &sh, uint64(100)); err != nil {
		return nil, err
	}
	if err := encoding.WriteObject(conn, modules.AcceptResponse); err != nil {
		return nil, err
	}
	return conn, nil
}

func DownloadBlocks(bchan chan *types.Block, conn io.ReadWriter, prevBlockID types.BlockID) (types.BlockID, error) {
	var rpcName [8]byte
	copy(rpcName[:], "SendBlocks")
	if err := encoding.WriteObject(conn, rpcName); err != nil {
		return prevBlockID, err
	}
	var history [32]types.BlockID
	history[31] = types.GenesisID
	moreAvailable := true
	// Send the block ids.
	history[0] = prevBlockID
	if err := encoding.WriteObject(conn, history); err != nil {
		return prevBlockID, err
	}
	for moreAvailable {
		// Read a slice of blocks from the wire.
		var newBlocks []types.Block
		if err := encoding.ReadObject(conn, &newBlocks, uint64(consensus.MaxCatchUpBlocks)*types.BlockSizeLimit); err != nil {
			return prevBlockID, err
		}
		if err := encoding.ReadObject(conn, &moreAvailable, 1); err != nil {
			return prevBlockID, err
		}
		log.Printf("moreAvailable = %v.", moreAvailable)
		for i := range newBlocks {
			b := &newBlocks[i]
			if b.ParentID != prevBlockID {
				return prevBlockID, fmt.Errorf("parent: %s, prev: %s", b.ParentID, prevBlockID)
			}
			log.Printf("Downloaded block %s.", b.ID())
			bchan <- b
			prevBlockID = b.ID()
		}
	}
	return prevBlockID, nil
}

func DownloadAllBlocks(bchan chan *types.Block, sess func() (io.ReadWriter, error)) error {
	prevBlockID := types.GenesisID
	for {
		stream, err := sess()
		if err != nil {
			return err
		}
		newPrevBlockID, err := DownloadBlocks(bchan, stream, prevBlockID)
		hadBlocks := newPrevBlockID != prevBlockID
		log.Printf("DownloadBlocks returned %v, %v.", hadBlocks, err)
		if err == nil || newPrevBlockID == prevBlockID {
			log.Printf("No error, all blocks were downloaded. Stopping.")
			break
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		prevBlockID = newPrevBlockID
	}
	return nil
}
