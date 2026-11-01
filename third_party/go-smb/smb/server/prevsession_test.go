// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid

package server_test

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfjallid/go-smb/ntlmssp"
	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/server"
	"github.com/jfjallid/go-smb/spnego"
)

// TestPreviousSessionEvictionScopedToConn drives two back-to-back
// SessionSetups on two different TCP connections, with the second one
// carrying PreviousSessionID = first session's ID, and verifies the first
// session SURVIVES.
//
// This server allocates SessionIDs per-Conn (Conn.nextSessionID restarts at
// 1 on every accepted socket), so an ID means nothing outside its own Conn.
// iOS opens one connection per share and remembers "last session id"
// globally, so it sends PreviousSessionID=1 on the second share's
// connection. Matching that across Conns used to delete the *first* share's
// live session — the phone then got STATUS_USER_SESSION_DELETED on it and
// rendered an empty, unusable folder. Eviction must stay inside one Conn.
//
// The in-tree client does not expose PreviousSessionID on Options, so we
// inject it via OnRawRequest after the second connection's NegotiateProtocol
// completes — patching the SessionSetup1Req body at body-offset 16 (packet
// offset 80) before dispatch sees it.
func TestPreviousSessionEvictionScopedToConn(t *testing.T) {
	const (
		user     = "alice"
		password = "Hunter2!"
		domain   = "WORKGROUP"
	)
	ntHash := ntlmssp.Ntowfv1(password)

	var capturedConn atomic.Pointer[server.Conn]
	var firstSessionID atomic.Uint64
	var secondSessionID atomic.Uint64
	var prevSessionToInject atomic.Uint64

	srv := &server.Server{
		Config: &server.ServerConfig{
			Authenticator: &server.MapAuthenticator{
				Domain:   domain,
				Accounts: map[string]*server.Account{user: {NTHash: ntHash}},
			},
			OnRawRequest: func(c *server.Conn, raw []byte) (bool, error) {
				// On the second connection, patch the PreviousSessionID
				// field of the first SessionSetup leg (NegTokenInit blob
				// starts with 0x60). Body starts at packet offset 64;
				// PreviousSessionID is at body offset 16 (packet 80).
				prev := prevSessionToInject.Load()
				if prev == 0 || capturedConn.Load() == c {
					return false, nil
				}
				if len(raw) < 88 || string(raw[0:4]) != smb.ProtocolSmb2 {
					return false, nil
				}
				cmd := binary.LittleEndian.Uint16(raw[12:14])
				if cmd != smb.CommandSessionSetup {
					return false, nil
				}
				// Only patch leg 1 (NegTokenInit, 0x60 tag in the blob).
				blobOff := int(binary.LittleEndian.Uint16(raw[64+12 : 64+14]))
				if blobOff == 0 || blobOff >= len(raw) {
					return false, nil
				}
				if raw[blobOff] != 0x60 {
					return false, nil
				}
				binary.LittleEndian.PutUint64(raw[80:88], prev)
				return false, nil
			},
			OnSessionSetup: func(c *server.Conn, s *server.Session, blob []byte, stage server.SessionSetupStage) (*server.Status, error) {
				if stage == server.SessionSetupStageAuthenticate {
					if firstSessionID.Load() == 0 {
						firstSessionID.Store(s.ID)
						capturedConn.Store(c)
					} else if secondSessionID.Load() == 0 {
						secondSessionID.Store(s.ID)
					}
				}
				return nil, nil
			},
		},
	}
	addr, shutdown := startTestServer(t, srv)
	defer shutdown()

	opts := smb.Options{
		Host:           "127.0.0.1",
		Port:           addr.Port,
		Initiator:      &spnego.NTLMInitiator{User: user, Password: password, Domain: domain},
		DisableSigning: true,
		Encryption:     smb.EncryptionDisabled,
		Dialects:       smb.DialectsSMB2Only,
		DialTimeout:    2 * time.Second,
	}
	c1, err := smb.NewConnection(opts)
	if err != nil {
		t.Fatalf("first NewConnection: %v", err)
	}
	defer c1.Close()
	if firstSessionID.Load() == 0 {
		t.Fatalf("first SessionSetup did not observe an authenticate stage")
	}

	// Arm the patcher with the first session's ID, then open a second
	// connection. Its SessionSetup1 body gets PreviousSessionID injected.
	prevSessionToInject.Store(firstSessionID.Load())
	c2, err := smb.NewConnection(opts)
	if err != nil {
		t.Fatalf("second NewConnection: %v", err)
	}
	defer c2.Close()
	if secondSessionID.Load() == 0 {
		t.Fatalf("second SessionSetup did not observe an authenticate stage")
	}

	// The first connection's session must still be there: its ID lives in a
	// different Conn's namespace, so a PreviousSessionID from another socket
	// cannot refer to it.
	cConn := capturedConn.Load()
	if cConn == nil {
		t.Fatalf("no Conn captured")
	}
	if !cConn.HasSessionForTest(firstSessionID.Load()) {
		t.Errorf("first session %d was evicted by a PreviousSessionID sent on another Conn", firstSessionID.Load())
	}
}
