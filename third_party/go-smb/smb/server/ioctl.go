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

	"github.com/jfjallid/go-smb/smb"
)

// IOCTL function codes the server recognizes. The named constants in
// smb.go are sparse — list the additional ones we handle here.
const (
	FsctlValidateNegotiateInfo uint32 = 0x00140204
	FsctlGetReparsePoint       uint32 = 0x000900a8
	FsctlQueryNetworkInterface uint32 = 0x001401fc
	// FsctlSrvRequestResumeKey（FSCTL_SRV_REQUEST_RESUME_KEY）是
	// 「给我这个文件的不透明标识」，客户端拿到后可以在
	// FSCTL_SRV_COPYCHUNK 里指认拷贝源。Apple 每次挂载共享根都会对它
	// 发一次（MaxOutputResponse=32、无输入），是继 QUERY_INFO 之后的
	// 第一个能力探测。回 STATUS_NOT_SUPPORTED 会被理解为「这台服务器
	// 不支持服务端语义」。
	FsctlSrvRequestResumeKey uint32 = 0x00140078
)

// buildResumeKey 构造 FSCTL_SRV_REQUEST_RESUME_KEY 的 24 字节应答
// （MS-SMB2 §2.2.32.3，与 Samba 的 file_id 三件套同尺寸）。
// 我们不实现服务端拷贝，所以只要求「同一文件恒定、不同文件不同」。
func buildResumeKey(h Handle) []byte {
	var id uint64
	if h != nil {
		if fi, err := h.Stat(); err == nil {
			id = fi.FileID
		}
	}
	out := make([]byte, 24)
	binary.LittleEndian.PutUint64(out[0:], id)
	copy(out[8:], "selfnas\x00")
	return out
}

// handleIoCtl processes SMB2 IOCTL. Three classes of behavior:
//
//   - FSCTL_VALIDATE_NEGOTIATE_INFO is built-in (Windows 10+ requires it).
//     The server echoes back the negotiated parameters so the client can
//     confirm the connection wasn't downgraded by an attacker.
//   - FSCTL_DFS_GET_REFERRALS is rejected with STATUS_FS_DRIVER_REQUIRED so
//     clients fall back to the local share.
//   - Everything else is delegated to VFS.Ioctl (default STATUS_NOT_SUPPORTED).
func (c *Conn) handleIoCtl(ctx pduCtx, raw []byte, h *smb.Header) error {
	logger := c.logger()

	sess := c.session(h.SessionID)
	if sess == nil || !sess.Authenticated {
		logger.Debugf("IoCtl: session %d not authenticated -> StatusUserSessionDeleted", h.SessionID)
		return c.writeRawError(ctx, h, smb.StatusUserSessionDeleted)
	}
	tree := sess.tree(h.TreeID)
	if tree == nil {
		logger.Debugf("IoCtl: unknown TreeID %d -> StatusNetworkNameDeleted", h.TreeID)
		return c.writeRawError(ctx, h, smb.StatusNetworkNameDeleted)
	}
	var req smb.IoCtlReq
	if err := req.UnmarshalBinary(raw); err != nil {
		logger.Errorf("IoCtl: decode IoCtlReq: %v", err)
		return formatErr("decode IoCtlReq", err)
	}

	logger.Debugf("IoCtl: tree=%d ctlCode=0x%08x maxOut=%d", tree.ID, req.CtlCode, req.MaxOutputResponse)

	switch req.CtlCode {
	case FsctlValidateNegotiateInfo:
		out := buildValidateNegotiateInfoOut(c)
		return c.writeIoCtlReply(ctx, h, &req, out, smb.StatusOk)
	case smb.FsctlDfsGetRefferrals:
		logger.Debugf("IoCtl: FsctlDfsGetReferrals -> statusFsDriverRequired (DFS not served)")
		return c.writeRawError(ctx, h, statusFsDriverRequired)
	}

	// File-handle-bound IOCTLs go to the handle's backend.
	hndl := tree.lookupHandle(volatileFromFileID(req.FileId))
	if hndl == nil {
		logger.Debugf("IoCtl from %s: tree=%d unknown volatileFID=%d -> StatusFileClosed",
			c.RemoteAddr, tree.ID, volatileFromFileID(req.FileId))
		return c.writeRawError(ctx, h, smb.StatusFileClosed)
	}
	if ph, ok := hndl.(*pipeHandle); ok {
		if req.CtlCode != smb.FsctlPipeTransceive {
			logger.Debugf("IoCtl from %s: pipe %q got non-Transceive ctlCode=0x%08x -> StatusNotSupported",
				c.RemoteAddr, ph.name, req.CtlCode)
			return c.writeRawError(ctx, h, smb.StatusNotSupported)
		}
		out, status, err := ph.backend.Transceive(context.Background(), req.Buffer)
		if err != nil {
			logger.Errorf("pipe %q Transceive: %v", ph.name, err)
			return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
		}
		if status != smb.StatusOk {
			logger.Debugf("IoCtl from %s: pipe %q Transceive returned status=0x%08x",
				c.RemoteAddr, ph.name, status)
			return c.writeRawError(ctx, h, status)
		}
		return c.writeIoCtlReply(ctx, h, &req, out, smb.StatusOk)
	}
	if tree.Share.VFS == nil {
		logger.Debugf("IoCtl from %s: share %q has nil VFS -> StatusNotSupported",
			c.RemoteAddr, tree.Share.Name)
		return c.writeRawError(ctx, h, smb.StatusNotSupported)
	}
	// 能力探测：给客户端一个稳定的 24 字节 resume key。
	// 不走 VFS —— 它只需要文件身份，任何 VFS 都能从 Stat 拿到。
	if req.CtlCode == FsctlSrvRequestResumeKey {
		logger.Debugf("IoCtl from %s: FSCTL_SRV_REQUEST_RESUME_KEY -> 24-byte key", c.RemoteAddr)
		return c.writeIoCtlReply(ctx, h, &req, buildResumeKey(hndl), smb.StatusOk)
	}
	out, status, err := tree.Share.VFS.Ioctl(context.Background(), hndl, req.CtlCode, req.Buffer, req.MaxOutputResponse)
	if err != nil {
		logger.Errorf("VFS.Ioctl 0x%08x: %v", req.CtlCode, err)
		return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
	}
	if status != smb.StatusOk {
		logger.Debugf("IoCtl from %s: VFS.Ioctl 0x%08x returned status=0x%08x",
			c.RemoteAddr, req.CtlCode, status)
		return c.writeRawError(ctx, h, status)
	}
	return c.writeIoCtlReply(ctx, h, &req, out, smb.StatusOk)
}

// writeIoCtlReply assembles an IoCtlRes carrying out as the OutputBuffer.
// The response leaves InputBuffer empty (server-to-client IOCTLs always do).
func (c *Conn) writeIoCtlReply(ctx pduCtx, reqHdr *smb.Header, req *smb.IoCtlReq, out []byte, status uint32) error {
	res := smb.IoCtlRes{
		Header:        buildResponseHeader(reqHdr, status, reqHdr.SessionID, smb.CommandIOCtl),
		StructureSize: 49,
		CtlCode:       req.CtlCode,
		FileId:        append([]byte(nil), req.FileId...),
		Flags:         0,
		Buffer:        out,
	}
	res.Header.TreeID = reqHdr.TreeID
	if len(res.FileId) != 16 {
		res.FileId = make([]byte, 16)
	}
	return c.writeReply(ctx, &res)
}

// buildValidateNegotiateInfoOut returns the FSCTL_VALIDATE_NEGOTIATE_INFO
// response body — Capabilities(4) + ServerGuid(16) + SecurityMode(2) +
// Dialect(2). The Capabilities and SecurityMode are mirrored verbatim from
// the values actually emitted in NegotiateRes (captured on Conn after the
// OnNegotiate hook); divergence from what the client received would cause
// the validate-negotiate check to fail and the connection to be torn down.
func buildValidateNegotiateInfoOut(c *Conn) []byte {
	out := make([]byte, 24)
	binary.LittleEndian.PutUint32(out[0:], c.NegotiatedCapabilities)
	guid := c.Server.resolvedServerGUID()
	copy(out[4:20], guid[:])
	binary.LittleEndian.PutUint16(out[20:], c.NegotiatedSecurityMode)
	binary.LittleEndian.PutUint16(out[22:], c.Dialect)
	return out
}

// statusFsDriverRequired (0xC000019C) — clients interpret this as "DFS not
// served here, use local share".
const statusFsDriverRequired uint32 = 0xC000019C
