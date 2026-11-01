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
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package server

import (
	"context"
	"encoding/binary"
	"strings"

	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/unicode"
)

// defaultMaximalAccess advertised when Share.MaximalAccess is zero. This is
// FILE_ALL_ACCESS (the value Windows servers return for an unrestricted
// share). Tighter caller-supplied values are honored.
const defaultMaximalAccess uint32 = 0x001f01ff

// handleTreeConnect parses an inbound TreeConnectReq, looks up the share by
// the trailing path component, allocates a Tree on the session, and replies
// with a populated TreeConnectRes.
func (c *Conn) handleTreeConnect(ctx pduCtx, raw []byte, h *smb.Header) error {
	cfg := c.Server.Config
	logger := c.logger()

	sess := c.session(h.SessionID)
	if sess == nil || !sess.Authenticated {
		logger.Debugf("TreeConnect: session %d missing or not authenticated -> StatusUserSessionDeleted", h.SessionID)
		return c.writeRawError(ctx, h, smb.StatusUserSessionDeleted)
	}

	var req smb.TreeConnectReq
	if err := req.UnmarshalBinary(raw); err != nil {
		logger.Errorf("TreeConnect: decode TreeConnectReq: %v", err)
		return formatErr("decode TreeConnectReq", err)
	}

	pathStr, err := unicode.FromUnicodeString(req.Path)
	if err != nil {
		logger.Errorf("TreeConnect: decode path: %v", err)
		return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
	}
	shareName := shareNameFromPath(pathStr)
	if shareName == "" {
		logger.Errorf("TreeConnect: empty share name in %q", pathStr)
		return c.writeRawError(ctx, h, smb.StatusBadNetworkName)
	}

	share, ok := cfg.lookupShare(shareName)
	if !ok {
		logger.Debugf("TreeConnect: unknown share %q from %s", shareName, c.RemoteAddr)
		return c.writeRawError(ctx, h, smb.StatusBadNetworkName)
	}

	// Allocate tree first so a hook can mutate state if it wants to.
	t := sess.addTree(share)

	res := smb.TreeConnectRes{
		Header:        buildResponseHeader(h, smb.StatusOk, h.SessionID, smb.CommandTreeConnect),
		StructureSize: 16,
		ShareType:     share.Type,
		ShareFlags:    shareFlagsFor(share),
		Capabilities:  share.Capabilities,
		// 按会话算：虚拟共享对「全只读账号」只宣告读权限，客户端不会把它挂成可写。
		MaximalAccess: shareMaximalAccess(share, sess),
	}
	res.Header.TreeID = t.ID

	// Apple's SMB client (iOS 18+/macOS) sends a create context named "AAPL"
	// in TREE_CONNECT and expects the server to answer with its capabilities.
	// If the server is silent, the client does not assume "no extensions" — it
	// assumes the share cannot safely store Apple metadata and **mounts it
	// read-only**, with every subsequent request still succeeding. That is the
	// documented iOS "只读" failure mode; Samba's fix is `vfs objects = fruit
	// streams_xattr`, where `fruit` is the piece that answers this context.
	if len(req.Context) > 0 {
		if reply := appleTreeConnectReply(req.Context); len(reply) > 0 {
			res.Context = reply
			logger.Debugf("TreeConnect from %s: AAPL context answered (%d bytes)", c.RemoteAddr, len(reply))
		} else {
			logger.Debugf("TreeConnect from %s: %d bytes of create contexts, no AAPL blob recognised",
				c.RemoteAddr, len(req.Context))
		}
	}

	if cb := cfg.OnTreeConnect; cb != nil {
		st, err := cb(c, sess, shareName, &req, &res)
		if err != nil {
			sess.removeTree(t.ID)
			return err
		}
		if st != nil {
			sess.removeTree(t.ID)
			return c.writeRawError(ctx, h, st.Code)
		}
	}

	logger.Debugf("TreeConnect from %s: SessionID=%d share=%q -> TreeID=%d",
		c.RemoteAddr, sess.ID, shareName, t.ID)

	return c.writeReply(ctx, &res)
}

// handleTreeDisconnect tears down the tree referenced by the request header.
// Open handles registered on the tree are closed via VFS.Close.
func (c *Conn) handleTreeDisconnect(ctx pduCtx, h *smb.Header) error {
	cfg := c.Server.Config
	logger := c.logger()

	sess := c.session(h.SessionID)
	if sess == nil {
		logger.Debugf("TreeDisconnect: unknown SessionID %d -> StatusUserSessionDeleted", h.SessionID)
		return c.writeRawError(ctx, h, smb.StatusUserSessionDeleted)
	}
	t := sess.removeTree(h.TreeID)
	if t == nil {
		logger.Debugf("TreeDisconnect: unknown TreeID %d on session %d -> StatusNetworkNameDeleted", h.TreeID, sess.ID)
		return c.writeRawError(ctx, h, smb.StatusNetworkNameDeleted)
	}

	if cb := cfg.OnTreeDisconnect; cb != nil {
		cb(c, sess, t)
	}

	t.closeOpenHandles(context.Background(), logger)

	logger.Debugf("TreeDisconnect: SessionID=%d TreeID=%d", sess.ID, t.ID)

	res := smb.TreeDisconnectRes{
		Header:        buildResponseHeader(h, smb.StatusOk, h.SessionID, smb.CommandTreeDisconnect),
		StructureSize: 4,
	}
	res.Header.TreeID = h.TreeID
	return c.writeReply(ctx, &res)
}

// shareNameFromPath returns the trailing component of an SMB path like
// "\\server\share" — i.e. "share". Inputs are case-preserved; lookup is
// case-insensitive elsewhere.
func shareNameFromPath(p string) string {
	p = strings.TrimRight(p, "\\")
	idx := strings.LastIndex(p, "\\")
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}

// shareFlagsFor returns the ShareFlags bits for the configured share. The
// default caching mode is ShareFlagManualCaching (zero) which is the most
// conservative; callers can override by setting Share.Capabilities (used as
// a free-form bag of flags is overkill — keep this simple for now).
func shareFlagsFor(s Share) uint32 {
	flags := smb.ShareFlagManualCaching
	if s.EncryptData {
		flags |= smb.ShareFlagEncryptData
	}
	return flags
}

// Apple extension constants for the *tree connect* AAPL context (see
// vfs_fruit.h in Samba). These are NOT the same encoding as the "AAPL"
// *create* context handled in createcontext_aapl.go: TREE_CONNECT carries a
// bare {command, reserved} header instead of a command + capability bitmaps.
const (
	aaplTCQuery = 1 // kAAPL_SERVER_QUERY — client asks for capabilities
	aaplTCCaps  = 2 // kAAPL_SERVER_CAPS  — server reports capabilities
)

// appleTreeConnectReply turns the client's "AAPL" tree-connect context into
// the server's answer.
//
// The reply is byte-for-byte the request context with only two things changed:
// the chain pointer (we always return a single context) and the 8-byte AAPL
// payload. Reusing the request's own context header means we inherit whatever
// name/data offsets and padding the client used, instead of guessing at them.
//
// The advertised capability bitmap is deliberately 0: we do not implement
// kAAPL_SUPPORTS_READ_DIR_ATTR here (macOS-specific AFP_AfpInfo attributes
// returned in QUERY_DIRECTORY) or kAAPL_SUPPORTS_OSX_COPYFILE (server-side
// copy). The *create* context path advertises READ_DIR_ATTR — that is what
// iOS keys on, and it is a different handshake. Note iOS never sends an AAPL
// context at TREE_CONNECT at all; this path exists for macOS.
func appleTreeConnectReply(reqCtx []byte) []byte {
	for off := 0; off+16 <= len(reqCtx); {
		nameOff := int(binary.LittleEndian.Uint16(reqCtx[off+4 : off+6]))
		nameLen := int(binary.LittleEndian.Uint16(reqCtx[off+6 : off+8]))
		dataOff := int(binary.LittleEndian.Uint16(reqCtx[off+10 : off+12]))
		dataLen := int(binary.LittleEndian.Uint32(reqCtx[off+12 : off+16]))

		if nameLen == 4 && nameOff+nameLen <= len(reqCtx)-off &&
			string(reqCtx[off+nameOff:off+nameOff+nameLen]) == createTagAAPL &&
			dataLen >= 8 && off+dataOff+8 <= len(reqCtx) {

			out := append([]byte(nil), reqCtx[off:off+dataOff+8]...)
			binary.LittleEndian.PutUint32(out[0:], 0) // no further contexts
			binary.LittleEndian.PutUint32(out[dataOff:], aaplTCCaps)
			binary.LittleEndian.PutUint32(out[dataOff+4:], 0)
			return out
		}

		next := int(binary.LittleEndian.Uint32(reqCtx[off : off+4]))
		if next <= 0 || off+next > len(reqCtx) {
			break
		}
		off += next
	}
	return nil
}
