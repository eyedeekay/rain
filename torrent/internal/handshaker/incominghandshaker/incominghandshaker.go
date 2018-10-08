package incominghandshaker

import (
	"io"
	"net"

	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/torrent/internal/bitfield"
	"github.com/cenkalti/rain/torrent/internal/btconn"
	"github.com/cenkalti/rain/torrent/internal/peerconn"
)

type IncomingHandshaker struct {
	conn     net.Conn
	bitfield *bitfield.Bitfield
	peerID   [20]byte
	sKeyHash [20]byte
	infoHash [20]byte
	resultC  chan Result
	closeC   chan struct{}
	doneC    chan struct{}
	log      logger.Logger
}

type Result struct {
	Peer  *peerconn.Conn
	Conn  net.Conn
	Error error
}

func NewIncoming(conn net.Conn, peerID, sKeyHash, infoHash [20]byte, resultC chan Result, l logger.Logger) *IncomingHandshaker {
	return &IncomingHandshaker{
		conn:     conn,
		peerID:   peerID,
		sKeyHash: sKeyHash,
		infoHash: infoHash,
		resultC:  resultC,
		closeC:   make(chan struct{}),
		doneC:    make(chan struct{}),
		log:      l,
	}
}

func (h *IncomingHandshaker) Close() {
	close(h.closeC)
	<-h.doneC
}

func (h *IncomingHandshaker) Run() {
	defer close(h.doneC)
	log := logger.New("peer <- " + h.conn.RemoteAddr().String())

	// TODO get this from config
	encryptionForceIncoming := false

	// TODO get supported extensions from common place
	ourExtensions := [8]byte{}
	ourbf := bitfield.NewBytes(ourExtensions[:], 64)
	ourbf.Set(61) // Fast Extension (BEP 6)
	ourbf.Set(43) // Extension Protocol (BEP 10)

	conn, cipher, peerExtensions, peerID, _, err := btconn.Accept(
		h.conn, h.getSKey, encryptionForceIncoming, h.checkInfoHash, ourExtensions, h.peerID)
	if err != nil {
		if err == io.EOF {
			log.Debug("peer has closed the connection: EOF")
		} else if err == io.ErrUnexpectedEOF {
			log.Debug("peer has closed the connection: Unexpected EOF")
		} else if _, ok := err.(*net.OpError); ok {
			log.Debugln("net operation error:", err)
		} else {
			log.Errorln("cannot complete incoming handshake:", err)
		}
		select {
		case h.resultC <- Result{Conn: h.conn, Error: err}:
		case <-h.closeC:
		}
		return
	}
	log.Debugf("Connection accepted. (cipher=%s extensions=%x client=%q)", cipher, peerExtensions, peerID[:8])

	peerbf := bitfield.NewBytes(peerExtensions[:], 64)
	extensions := ourbf.And(peerbf)

	p := peerconn.New(conn, peerID, extensions, log)
	select {
	case h.resultC <- Result{Conn: h.conn, Peer: p}:
	case <-h.closeC:
	}
}

func (h *IncomingHandshaker) getSKey(sKeyHash [20]byte) []byte {
	if sKeyHash == h.sKeyHash {
		return h.infoHash[:]
	}
	return nil
}

func (h *IncomingHandshaker) checkInfoHash(infoHash [20]byte) bool {
	return infoHash == h.infoHash
}