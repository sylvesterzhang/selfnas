// MIT License
//
// Copyright (c) 2026 Jimmy Fjällid
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

package server

// This file uses the standard Go "export_test.go" pattern: it is part of
// package server but, ending in _test.go, is compiled only for tests. It
// re-exports internals needed by the external server_test package without
// leaking them into the production build.

// DeriveSigningKey311ForTest is a test-only re-export of the 3.1.1 signing
// key KDF: KDF(sessionKey, "SMBSigningKey", preauthHash). Tests use it to
// confirm divergent preauth chains yield divergent keys (the implicit
// anti-downgrade property; see negotiate.go).
func DeriveSigningKey311ForTest(sessionKey, preauthHash []byte) []byte {
	return kdfHmacSha256(sessionKey, []byte("SMBSigningKey\x00"), preauthHash, 128)
}

// HasSessionForTest reports whether the Conn still tracks a session with the
// given ID. Tests use it to assert a session survived (or was evicted) without
// mutating the table the way RemoveSession would.
func (c *Conn) HasSessionForTest(id uint64) bool {
	return c.session(id) != nil
}

// DefaultFsInfoForTest is a test-only re-export of the synthetic filesystem
// info builder, so tests can assert on the exact bytes clients receive.
func DefaultFsInfoForTest(class byte, label string) ([]byte, uint32) {
	return defaultFsInfo(class, label)
}

// DefaultSecurityDescriptorForTest re-exports the fallback SD so tests can
// parse it back and prove it is well-formed.
func DefaultSecurityDescriptorForTest() []byte {
	return worldReadableSecurityDescriptor()
}

// AppleTreeConnectReplyForTest re-exports the AAPL context builder.
func AppleTreeConnectReplyForTest(ctx []byte) []byte {
	return appleTreeConnectReply(ctx)
}

// SendUnsignedForTest is a test-only re-export of the raw, unsigned send path.
// Tests use it to inject an unsolicited oplock break the way Windows sends one:
// on the reserved MessageId and without SMB2_FLAGS_SIGNED, even when the
// session requires signing.
func (c *Conn) SendUnsignedForTest(buf []byte) error {
	return c.sendPacketUnsigned(buf)
}
