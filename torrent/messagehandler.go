package torrent

import (
	"bytes"
	"crypto/sha1" // nolint: gosec
	"errors"
	"fmt"

	"github.com/cenkalti/rain/torrent/internal/bitfield"
	"github.com/cenkalti/rain/torrent/internal/peer"
	"github.com/cenkalti/rain/torrent/internal/peerconn/peerreader"
	"github.com/cenkalti/rain/torrent/internal/peerconn/peerwriter"
	"github.com/cenkalti/rain/torrent/internal/peerprotocol"
	"github.com/cenkalti/rain/torrent/internal/piecewriter"
	"github.com/cenkalti/rain/torrent/internal/tracker"
	"github.com/cenkalti/rain/torrent/metainfo"
)

func (t *Torrent) handlePeerMessage(pm peer.Message) {
	pe := pm.Peer
	switch msg := pm.Message.(type) {
	case peerprotocol.HaveMessage:
		// Save have messages for processesing later received while we don't have info yet.
		if t.pieces == nil || t.bitfield == nil {
			pe.Messages = append(pe.Messages, msg)
			break
		}
		if msg.Index >= t.info.NumPieces {
			pe.Logger().Errorln("unexpected piece index:", msg.Index)
			t.closePeer(pe)
			break
		}
		pi := &t.pieces[msg.Index]
		// pe.Logger().Debug("Peer ", pe.String(), " has piece #", pi.Index)
		t.piecePicker.HandleHave(pe, pi.Index)
		t.updateInterestedState(pe)
		t.startPieceDownloaders()
	case peerprotocol.BitfieldMessage:
		// Save bitfield messages while we don't have info yet.
		if t.pieces == nil || t.bitfield == nil {
			pe.Messages = append(pe.Messages, msg)
			break
		}
		numBytes := uint32(bitfield.NumBytes(t.info.NumPieces))
		if uint32(len(msg.Data)) != numBytes {
			pe.Logger().Errorln("invalid bitfield length:", len(msg.Data))
			t.closePeer(pe)
			break
		}
		bf := bitfield.NewBytes(msg.Data, t.info.NumPieces)
		pe.Logger().Debugln("Received bitfield:", bf.Hex())
		for i := uint32(0); i < bf.Len(); i++ {
			if bf.Test(i) {
				t.piecePicker.HandleHave(pe, i)
			}
		}
		t.updateInterestedState(pe)
		t.startPieceDownloaders()
	case peerprotocol.HaveAllMessage:
		if t.pieces == nil || t.bitfield == nil {
			pe.Messages = append(pe.Messages, msg)
			break
		}
		for _, pi := range t.pieces {
			t.piecePicker.HandleHave(pe, pi.Index)
		}
		t.updateInterestedState(pe)
		t.startPieceDownloaders()
	case peerprotocol.HaveNoneMessage: // TODO handle?
	case peerprotocol.AllowedFastMessage:
		if t.pieces == nil || t.bitfield == nil {
			pe.Messages = append(pe.Messages, msg)
			break
		}
		if msg.Index >= t.info.NumPieces {
			pe.Logger().Errorln("invalid allowed fast piece index:", msg.Index)
			t.closePeer(pe)
			break
		}
		pi := &t.pieces[msg.Index]
		pe.Logger().Debug("Peer ", pe.String(), " has allowed fast for piece #", pi.Index)
		t.piecePicker.HandleAllowedFast(pe, msg.Index)
	case peerprotocol.UnchokeMessage:
		pe.PeerChoking = false
		if pd, ok := t.pieceDownloaders[pe]; ok {
			pd.RequestBlocks(t.config.RequestQueueLength)
		}
		t.startPieceDownloaders()
	case peerprotocol.ChokeMessage:
		pe.PeerChoking = true
		if pd, ok := t.pieceDownloaders[pe]; ok {
			pd.Choked()
			t.pieceDownloadersChoked[pe] = pd
			t.startPieceDownloaders()
		}
	case peerprotocol.InterestedMessage:
		// TODO handle intereseted messages
	case peerprotocol.NotInterestedMessage:
		// TODO handle not intereseted messages
	case peerreader.Piece:
		if t.pieces == nil || t.bitfield == nil {
			pe.Logger().Error("piece received but we don't have info")
			t.closePeer(pe)
			break
		}
		if msg.Index >= uint32(len(t.pieces)) {
			pe.Logger().Errorln("invalid piece index:", msg.Index)
			t.closePeer(pe)
			break
		}
		piece := &t.pieces[msg.Index]
		block := piece.Blocks.Find(msg.Begin, uint32(len(msg.Data)))
		if block == nil {
			pe.Logger().Errorln("invalid piece begin:", msg.Begin, "length:", len(msg.Data))
			t.closePeer(pe)
			break
		}
		pe.BytesDownlaodedInChokePeriod += int64(len(msg.Data))
		t.bytesDownloaded += int64(len(msg.Data))
		pd, ok := t.pieceDownloaders[pe]
		if !ok {
			t.bytesWasted += int64(len(msg.Data))
			break
		}
		if pd.Piece.Index != msg.Index {
			t.bytesWasted += int64(len(msg.Data))
			break
		}
		pd.GotBlock(block, msg.Data)
		if !pd.Done() {
			pd.RequestBlocks(t.config.RequestQueueLength)
			pe.StartSnubTimer(t.config.RequestTimeout, t.peerSnubbedC)
			break
		}
		// t.log.Debugln("piece download completed. index:", pd.Piece.Index)
		t.closePieceDownloader(pd)
		pe.StopSnubTimer()

		ok = piece.VerifyHash(pd.Bytes, sha1.New()) // nolint: gosec
		if !ok {
			// TODO handle corrupt piece
			t.log.Error("received corrupt piece")
			t.closePeer(pd.Peer)
			t.startPieceDownloaders()
			break
		}

		for pe := range t.piecePicker.RequestedPeers(piece.Index) {
			pd2 := t.pieceDownloaders[pe]
			t.closePieceDownloader(pd2)
			pd2.CancelPending()
		}

		t.piecePicker.HandleWriting(piece.Index)
		pw := piecewriter.New(piece)
		t.pieceWriters[pw] = struct{}{}
		go pw.Run(pd.Bytes, t.pieceWriterResultC)

		t.startPieceDownloaders()
	case peerprotocol.RequestMessage:
		if t.pieces == nil || t.bitfield == nil {
			pe.Logger().Error("request received but we don't have info")
			t.closePeer(pe)
			break
		}
		if msg.Index >= t.info.NumPieces {
			pe.Logger().Errorln("invalid request index:", msg.Index)
			t.closePeer(pe)
			break
		}
		if msg.Begin+msg.Length > t.pieces[msg.Index].Length {
			pe.Logger().Errorln("invalid request length:", msg.Length)
			t.closePeer(pe)
			break
		}
		pi := &t.pieces[msg.Index]
		if pe.AmChoking {
			if pe.FastExtension {
				m := peerprotocol.RejectMessage{RequestMessage: msg}
				pe.SendMessage(m)
			}
		} else {
			pe.SendPiece(msg, pi.Data)
		}
	case peerprotocol.RejectMessage:
		if t.pieces == nil || t.bitfield == nil {
			pe.Logger().Error("reject received but we don't have info")
			t.closePeer(pe)
			break
		}

		if msg.Index >= t.info.NumPieces {
			pe.Logger().Errorln("invalid reject index:", msg.Index)
			t.closePeer(pe)
			break
		}
		piece := &t.pieces[msg.Index]
		block := piece.Blocks.Find(msg.Begin, msg.Length)
		if block == nil {
			pe.Logger().Errorln("invalid reject begin:", msg.Begin, "length:", msg.Length)
			t.closePeer(pe)
			break
		}
		// TODO check piece index
		// TODO check piece index on cancel
		pd, ok := t.pieceDownloaders[pe]
		if !ok {
			pe.Logger().Error("reject received but we don't have active download")
			t.closePeer(pe)
			break
		}
		pd.Rejected(block)
	case peerwriter.BlockUploaded:
		t.bytesUploaded += int64(msg.Length)
		pe.BytesUploadedInChokePeriod += int64(msg.Length)
	// TODO make extension messages value type
	case *peerprotocol.ExtensionHandshakeMessage:
		pe.Logger().Debugln("extension handshake received:", msg)
		if pe.ExtensionHandshake != nil {
			pe.Logger().Debugln("peer changed extensions")
			break
		}
		pe.ExtensionHandshake = msg

		if _, ok := msg.M[peerprotocol.ExtensionKeyMetadata]; ok {
			t.startInfoDownloaders()
		}
		if _, ok := msg.M[peerprotocol.ExtensionKeyPEX]; ok {
			if t.info != nil && t.info.Private != 1 {
				pe.StartPEX(t.peers)
			}
		}
	case *peerprotocol.ExtensionMetadataMessage:
		switch msg.Type {
		case peerprotocol.ExtensionMetadataMessageTypeRequest:
			extMsgID, ok := pe.ExtensionHandshake.M[peerprotocol.ExtensionKeyMetadata]
			if !ok {
				break
			}
			if t.info == nil {
				// Send reject
				dataMsg := peerprotocol.ExtensionMetadataMessage{
					Type:  peerprotocol.ExtensionMetadataMessageTypeReject,
					Piece: msg.Piece,
				}
				extDataMsg := peerprotocol.ExtensionMessage{
					ExtendedMessageID: extMsgID,
					Payload:           &dataMsg,
				}
				pe.SendMessage(extDataMsg)
				break
			}
			if msg.Piece >= uint32(len(t.pieces)) {
				pe.Logger().Errorln("peer requested invalid metadata piece:", msg.Piece)
				t.closePeer(pe)
				break
			}
			// TODO Clients MAY implement flood protection by rejecting request messages after a certain number of them have been served. Typically the number of pieces of metadata times a factor.
			start := 16 * 1024 * msg.Piece
			end := 16 * 1024 * (msg.Piece + 1)
			totalSize := uint32(len(t.info.Bytes))
			if end > totalSize {
				end = totalSize
			}
			data := t.info.Bytes[start:end]
			dataMsg := peerprotocol.ExtensionMetadataMessage{
				Type:      peerprotocol.ExtensionMetadataMessageTypeData,
				Piece:     msg.Piece,
				TotalSize: totalSize,
				Data:      data,
			}
			extDataMsg := peerprotocol.ExtensionMessage{
				ExtendedMessageID: extMsgID,
				Payload:           &dataMsg,
			}
			pe.SendMessage(extDataMsg)
		case peerprotocol.ExtensionMetadataMessageTypeData:
			id, ok := t.infoDownloaders[pe]
			if !ok {
				break
			}
			err := id.GotBlock(msg.Piece, msg.Data)
			if err != nil {
				pe.Logger().Error(err)
				t.closePeer(pe)
				t.startInfoDownloaders()
				break
			}
			if !id.Done() {
				id.RequestBlocks(t.config.RequestQueueLength)
				pe.StartSnubTimer(t.config.RequestTimeout, t.peerSnubbedC)
				break
			}
			pe.StopSnubTimer()

			hash := sha1.New()                              // nolint: gosec
			hash.Write(id.Bytes)                            // nolint: gosec
			if !bytes.Equal(hash.Sum(nil), t.infoHash[:]) { // nolint: gosec
				pe.Logger().Errorln("received info does not match with hash")
				t.closePeer(id.Peer)
				t.startInfoDownloaders()
				break
			}
			t.stopInfoDownloaders()

			t.info, err = metainfo.NewInfo(id.Bytes)
			if err != nil {
				err = fmt.Errorf("cannot parse info bytes: %s", err)
				t.log.Error(err)
				t.stop(err)
				break
			}
			if t.info.Private == 1 {
				err = errors.New("private torrent from magnet")
				t.log.Error(err)
				t.stop(err)
				break
			}
			if t.resume != nil {
				err = t.resume.WriteInfo(t.info.Bytes)
				if err != nil {
					err = fmt.Errorf("cannot write resume info: %s", err)
					t.log.Error(err)
					t.stop(err)
					break
				}
			}
			t.startAllocator()
		case peerprotocol.ExtensionMetadataMessageTypeReject:
			id, ok := t.infoDownloaders[pe]
			if ok {
				t.closePeer(id.Peer)
				t.startInfoDownloaders()
			}
		}
	case *peerprotocol.ExtensionPEXMessage:
		addrs, err := tracker.DecodePeersCompact([]byte(msg.Added))
		if err != nil {
			t.log.Error(err)
			break
		}
		t.handleNewPeers(addrs, "pex")
	default:
		panic(fmt.Sprintf("unhandled peer message type: %T", msg))
	}
}

func (t *Torrent) updateInterestedState(pe *peer.Peer) {
	if t.pieces == nil || t.bitfield == nil {
		return
	}
	interested := false
	for i := uint32(0); i < t.bitfield.Len(); i++ {
		weHave := t.bitfield.Test(i)
		peerHave := t.piecePicker.DoesHave(pe, i)
		if !weHave && peerHave {
			interested = true
			break
		}
	}
	if !pe.AmInterested && interested {
		pe.AmInterested = true
		msg := peerprotocol.InterestedMessage{}
		pe.SendMessage(msg)
		return
	}
	if pe.AmInterested && !interested {
		pe.AmInterested = false
		msg := peerprotocol.NotInterestedMessage{}
		pe.SendMessage(msg)
		return
	}
}
