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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/jfjallid/go-smb/smb"
	"github.com/jfjallid/go-smb/smb/unicode"
)

// FileFsAttributeInformation flags. Only the ones we can actually honour are
// listed here; notably FILE_READ_ONLY_VOLUME is NOT among them — advertising
// it makes Apple clients mount the share read-only.
//
// PATCH(selfnas): FILE_NAMED_STREAMS is advertised even though filevfs only
// supports streams incidentally (native NTFS ADS). Apple's iOS/macOS SMB
// client treats "server does not declare alternate data streams" as a
// metadata-unsafe server and falls back to a *read-only mount* — which is
// exactly the symptom that cost us a long debugging session: everything at
// the protocol layer returns 0x00000000, yet the phone shows 只读 under the
// location title. Samba has the same problem and the documented fix is
// `vfs objects = fruit streams_xattr`, i.e. declaring stream support.
const (
	FsAttrCaseSensitiveSearch = 0x00000001
	FsAttrCasePreservedNames  = 0x00000002
	FsAttrUnicodeOnDisk       = 0x00000004
	// FsAttrPersistentACLs tells the client "the security descriptor I hand you
	// is meaningful and durable". Apple's client only bothers to derive a
	// node's POSIX permission bits from the SD when this is set; without it the
	// root node ends up with a synthetic all-zero mode, and the mount inherits
	// "nobody can write".
	FsAttrPersistentACLs = 0x00000008
	// Sparse files and reparse points round the set out to exactly what Samba
	// reports for a stock writable share (0x000400cf). Byte-for-byte parity
	// with a known-good server is the cheapest defence when the client's
	// heuristic is undocumented.
	FsAttrSupportsSparseFiles   = 0x00000040
	FsAttrSupportsReparsePoints = 0x00000080
	FsAttrNamedStreams          = 0x00040000
	FsAttrReadOnlyVolume        = 0x00080000
)

// fsAttrBits 返回我们对 FileFsAttributeInformation 的固定口径。
// 抽出来是因为 "AAPL" create context 的应答也要按同一份能力位决定
// 是否声明 SUPPORTS_READ_DIR_ATTR —— 两处口径必须同源，否则会出现
// 「告诉了客户端有命名流、却又说没有 readdir attr」这种自相矛盾的组合。
func fsAttrBits() uint32 {
	return uint32(FsAttrCaseSensitiveSearch | FsAttrCasePreservedNames |
		FsAttrUnicodeOnDisk | FsAttrPersistentACLs | FsAttrSupportsSparseFiles |
		FsAttrSupportsReparsePoints | FsAttrNamedStreams)
}

// handleQueryInfo processes SMB2 QUERY_INFO. Three InfoTypes are supported:
//
//   - OInfoFile (0x01) — per-handle file info classes (Basic, Standard,
//     NetworkOpen, EndOfFile, AllInformation, etc.)
//   - OInfoFilesystem (0x02) — volume / attribute info classes
//   - OInfoSecurity (0x03) — security descriptor for the handle's object
//
// VFS hooks see the same InfoType / FileInfoClass byte and decide whether
// to fulfil the request themselves; if they return STATUS_NOT_SUPPORTED the
// server falls back to a default response built from the handle's Stat().
func (c *Conn) handleQueryInfo(ctx pduCtx, raw []byte, h *smb.Header) error {
	logger := c.logger()

	sess := c.session(h.SessionID)
	if sess == nil || !sess.Authenticated {
		logger.Debugf("QueryInfo: session %d not authenticated -> StatusUserSessionDeleted", h.SessionID)
		return c.writeRawError(ctx, h, smb.StatusUserSessionDeleted)
	}
	tree := sess.tree(h.TreeID)
	if tree == nil {
		logger.Debugf("QueryInfo: unknown TreeID %d -> StatusNetworkNameDeleted", h.TreeID)
		return c.writeRawError(ctx, h, smb.StatusNetworkNameDeleted)
	}
	var req smb.QueryInfoReq
	if err := req.UnmarshalBinary(raw); err != nil {
		logger.Errorf("QueryInfo: decode QueryInfoReq: %v", err)
		return formatErr("decode QueryInfoReq", err)
	}
	hndl := tree.lookupHandle(volatileFromFileID(req.FileId))
	// FILESYSTEM info works without a per-file handle in some clients, but
	// real Windows always passes a valid FID — reject if missing.
	if hndl == nil {
		logger.Debugf("QueryInfo from %s: tree=%d unknown volatileFID=%d -> StatusFileClosed",
			c.RemoteAddr, tree.ID, volatileFromFileID(req.FileId))
		return c.writeRawError(ctx, h, smb.StatusFileClosed)
	}

	switch req.InfoType {
	case smb.OInfoFile:
		return c.queryFileInfo(ctx, h, tree, hndl, &req)
	case smb.OInfoFilesystem:
		return c.queryFsInfo(ctx, h, tree, &req)
	case smb.OInfoSecurity:
		return c.querySecurityInfo(ctx, h, tree, hndl, &req)
	default:
		logger.Debugf("QueryInfo from %s: unsupported InfoType 0x%02x -> StatusNotSupported",
			c.RemoteAddr, req.InfoType)
		return c.writeRawError(ctx, h, smb.StatusNotSupported)
	}
}

func (c *Conn) queryFileInfo(ctx pduCtx, h *smb.Header, tree *Tree, hndl Handle, req *smb.QueryInfoReq) error {
	logger := c.logger()
	// Defer to VFS first; it may return a serialized buffer or struct.
	out, status, err := tree.Share.VFS.QueryFileInfo(context.Background(), hndl, req.FileInfoClass)
	if err != nil {
		logger.Errorf("VFS.QueryFileInfo class=0x%02x: %v", req.FileInfoClass, err)
		return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
	}
	var buf []byte
	if status == smb.StatusNotSupported {
		// Fall back to server-built default from Stat().
		info, statErr := hndl.Stat()
		if statErr != nil {
			logger.Errorf("QueryFileInfo: handle Stat: %v", statErr)
			return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
		}
		buf, status = serializeFileInfo(req.FileInfoClass, info, hndl)
		if status != smb.StatusOk {
			logger.Debugf("QueryFileInfo: serializeFileInfo class=0x%02x returned status=0x%08x",
				req.FileInfoClass, status)
		}
	} else if status == smb.StatusOk {
		buf, err = serializeOpaqueOrInfo(out)
		if err != nil {
			logger.Errorf("QueryFileInfo: serializeOpaqueOrInfo: %v", err)
			return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
		}
	}
	if status != smb.StatusOk {
		return c.writeRawError(ctx, h, status)
	}
	// Which class the client asked for and what we answered is the only way to
	// tell apart "the client is unhappy about a value" from "the client never
	// asked". Keep it at Debug; it is one line per QUERY_INFO.
	logger.Debugf("QueryInfo file from %s: class=0x%02x path=%q len=%d hex=%s",
		c.RemoteAddr, req.FileInfoClass, hndl.Path(), len(buf), hexPreview(buf, 48))
	return c.writeQueryInfoReply(ctx, h, buf)
}

func (c *Conn) queryFsInfo(ctx pduCtx, h *smb.Header, tree *Tree, req *smb.QueryInfoReq) error {
	logger := c.logger()
	out, status, err := tree.Share.VFS.QueryFSInfo(context.Background(), req.FileInfoClass)
	if err != nil {
		logger.Errorf("VFS.QueryFSInfo class=0x%02x: %v", req.FileInfoClass, err)
		return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
	}
	var buf []byte
	if status == smb.StatusNotSupported {
		buf, status = defaultFsInfo(req.FileInfoClass, tree.Share.Name)
		if status != smb.StatusOk {
			logger.Debugf("QueryFSInfo: defaultFsInfo class=0x%02x returned status=0x%08x",
				req.FileInfoClass, status)
		}
	} else if status == smb.StatusOk {
		buf, err = serializeOpaqueOrInfo(out)
		if err != nil {
			logger.Errorf("QueryFSInfo: serializeOpaqueOrInfo: %v", err)
			return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
		}
	}
	if status != smb.StatusOk {
		return c.writeRawError(ctx, h, status)
	}
	logger.Debugf("QueryInfo fs from %s: share=%q class=0x%02x len=%d hex=%s",
		c.RemoteAddr, tree.Share.Name, req.FileInfoClass, len(buf), hexPreview(buf, 48))
	return c.writeQueryInfoReply(ctx, h, buf)
}

// hexPreview renders at most n bytes of b as hex, for Debug logs.
func hexPreview(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return hex.EncodeToString(b[:n])
}

func (c *Conn) querySecurityInfo(ctx pduCtx, h *smb.Header, tree *Tree, hndl Handle, req *smb.QueryInfoReq) error {
	logger := c.logger()
	buf, status, err := tree.Share.VFS.QuerySecurity(context.Background(), hndl, req.AdditionalInformation)
	if err != nil {
		logger.Errorf("VFS.QuerySecurity addInfo=0x%08x: %v", req.AdditionalInformation, err)
		return c.writeRawError(ctx, h, smb.StatusInvalidParameter)
	}
	if status == smb.StatusNotSupported {
		// Default SD granting Everyone FILE_ALL_ACCESS through a real DACL.
		buf = worldReadableSecurityDescriptor()
		status = smb.StatusOk
	}
	if status != smb.StatusOk {
		logger.Debugf("QuerySecurityInfo: VFS returned status=0x%08x", status)
		return c.writeRawError(ctx, h, status)
	}
	// Apple's client derives the mount's read/write state from the root's
	// security descriptor, so when it asks we want it in the log.
	logger.Debugf("QueryInfo security from %s: addInfo=0x%08x path=%q len=%d hex=%s",
		c.RemoteAddr, req.AdditionalInformation, hndl.Path(), len(buf), hexPreview(buf, 48))
	return c.writeQueryInfoReply(ctx, h, buf)
}

// writeQueryInfoReply marshals a QueryInfoRes carrying buf and sends it.
func (c *Conn) writeQueryInfoReply(ctx pduCtx, reqHdr *smb.Header, buf []byte) error {
	res := smb.QueryInfoRes{
		Header:        buildResponseHeader(reqHdr, smb.StatusOk, reqHdr.SessionID, smb.CommandQueryInfo),
		StructureSize: 9,
		Buffer:        buf,
	}
	res.Header.TreeID = reqHdr.TreeID
	return c.writeReply(ctx, &res)
}

// serializeOpaqueOrInfo accepts either a []byte (already-serialized) or one
// of the typed Information structs and returns its wire bytes.
func serializeOpaqueOrInfo(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return nil, fmt.Errorf("VFS returned unrecognized info type %T", v)
}

// serializeFileInfo serializes a FileInfo into the requested FILE_*INFORMATION
// wire format. Returns the buffer plus an NT status (StatusOk on success,
// StatusInfoLengthMismatch / StatusNotSupported otherwise).
func serializeFileInfo(class byte, fi FileInfo, hndl Handle) ([]byte, uint32) {
	switch class {
	case smb.FileBasicInformation: // 0x04, 40 bytes
		buf := make([]byte, 40)
		binary.LittleEndian.PutUint64(buf[0:], timeToFileTime(fi.CreationTime))
		binary.LittleEndian.PutUint64(buf[8:], timeToFileTime(fi.LastAccessTime))
		binary.LittleEndian.PutUint64(buf[16:], timeToFileTime(fi.LastWriteTime))
		binary.LittleEndian.PutUint64(buf[24:], timeToFileTime(fi.ChangeTime))
		binary.LittleEndian.PutUint32(buf[32:], fi.Attributes)
		// reserved 4 bytes already zero
		return buf, smb.StatusOk

	case smb.FileStandardInformation: // 0x05, 24 bytes
		buf := make([]byte, 24)
		binary.LittleEndian.PutUint64(buf[0:], uint64(fi.AllocationSize))
		binary.LittleEndian.PutUint64(buf[8:], uint64(fi.Size))
		binary.LittleEndian.PutUint32(buf[16:], 1) // NumberOfLinks
		// DeletePending(1) + Directory(1) + Reserved(2)
		if hndl != nil && hndl.IsDir() {
			buf[21] = 1
		}
		return buf, smb.StatusOk

	case smb.FileEaInformation: // 0x07, 4 bytes (EaSize)
		return make([]byte, 4), smb.StatusOk

	// PATCH(selfnas): 0x06 — FILE_INTERNAL_INFORMATION, 8 字节 IndexNumber。
	// Windows 资源管理器打开目录后会问这个（它拿来做去重/增量刷新）。
	// 以前落到 default 回 NOT_SUPPORTED，资源管理器直接报「参数错误」。
	case smb.FileInternalInformation: // 0x06, 8 bytes
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf[0:], fi.FileID)
		return buf, smb.StatusOk

	case smb.FileNameInformation: // 0x09 — name relative to share root
		var nameBytes []byte
		if hndl != nil {
			nameBytes = unicode.ToUnicode("\\" + hndl.Path())
		}
		buf := make([]byte, 4+len(nameBytes))
		binary.LittleEndian.PutUint32(buf[0:], uint32(len(nameBytes)))
		copy(buf[4:], nameBytes)
		return buf, smb.StatusOk

	case smb.FileNetworkOpenInformation: // 0x22, 56 bytes
		buf := make([]byte, 56)
		binary.LittleEndian.PutUint64(buf[0:], timeToFileTime(fi.CreationTime))
		binary.LittleEndian.PutUint64(buf[8:], timeToFileTime(fi.LastAccessTime))
		binary.LittleEndian.PutUint64(buf[16:], timeToFileTime(fi.LastWriteTime))
		binary.LittleEndian.PutUint64(buf[24:], timeToFileTime(fi.ChangeTime))
		binary.LittleEndian.PutUint64(buf[32:], uint64(fi.AllocationSize))
		binary.LittleEndian.PutUint64(buf[40:], uint64(fi.Size))
		binary.LittleEndian.PutUint32(buf[48:], fi.Attributes)
		return buf, smb.StatusOk

	case smb.FileAllInformation: // 0x12 — Basic+Standard+Internal+Ea+Access+Position+Mode+Alignment+Name
		var b bytes.Buffer
		basic, status := serializeFileInfo(smb.FileBasicInformation, fi, hndl)
		if status != smb.StatusOk {
			log.Debugf("serializeFileInfo(FileAllInformation): basic sub-class failed -> 0x%08x", status)
			return nil, status
		}
		std, status := serializeFileInfo(smb.FileStandardInformation, fi, hndl)
		if status != smb.StatusOk {
			log.Debugf("serializeFileInfo(FileAllInformation): standard sub-class failed -> 0x%08x", status)
			return nil, status
		}
		b.Write(basic)
		b.Write(std)
		// FILE_INTERNAL_INFORMATION: IndexNumber (8 bytes)
		var idx [8]byte
		binary.LittleEndian.PutUint64(idx[:], fi.FileID)
		b.Write(idx[:])
		// FILE_EA_INFORMATION: EaSize (4)
		b.Write(make([]byte, 4))
		// FILE_ACCESS_INFORMATION: AccessFlags (4)
		var acc [4]byte
		binary.LittleEndian.PutUint32(acc[:], 0x001f01ff) // FILE_ALL_ACCESS
		b.Write(acc[:])
		// FILE_POSITION_INFORMATION: CurrentByteOffset (8)
		b.Write(make([]byte, 8))
		// FILE_MODE_INFORMATION: Mode (4)
		b.Write(make([]byte, 4))
		// FILE_ALIGNMENT_INFORMATION: AlignmentRequirement (4)
		b.Write(make([]byte, 4))
		// FILE_NAME_INFORMATION
		nameInfo, status := serializeFileInfo(smb.FileNameInformation, fi, hndl)
		if status != smb.StatusOk {
			log.Debugf("serializeFileInfo(FileAllInformation): name sub-class failed -> 0x%08x", status)
			return nil, status
		}
		b.Write(nameInfo)
		return b.Bytes(), smb.StatusOk

	case smb.FilePositionInformation: // 0x0e — 8 bytes CurrentByteOffset
		return make([]byte, 8), smb.StatusOk

	case smb.FileAlignmentInformation: // 0x11 — 4 bytes AlignmentRequirement
		return make([]byte, 4), smb.StatusOk

	case smb.FileStreamInformation: // 0x16 — 命名流列表
		// PATCH(selfnas): 之前回 NOT_SUPPORTED。第三方客户端（含 iOS App）
		// 会用它枚举文件的流（缩略图/元数据都放在 ADS 里），拿不到就当
		// 文件不可读处理。这里永远只报主流 "::$DATA"，大小即文件大小；
		// 不去枚举真实 ADS——synthetic stream 本来就兜底成空流，
		// 列不全无害，列不出来才有害。
		if hndl != nil && hndl.IsDir() {
			return nil, smb.StatusOk // 目录没有数据流
		}
		name := unicode.ToUnicode("::$DATA")
		entry := 24 + len(name)
		aligned := (entry + 7) &^ 7 // NextEntryOffset 必须 8 字节对齐
		buf := make([]byte, aligned)
		binary.LittleEndian.PutUint32(buf[0:], 0) // 最后一条，无下一条
		binary.LittleEndian.PutUint32(buf[4:], uint32(len(name)))
		binary.LittleEndian.PutUint64(buf[8:], uint64(fi.Size))
		binary.LittleEndian.PutUint64(buf[16:], uint64(fi.AllocationSize))
		copy(buf[24:], name)
		return buf, smb.StatusOk

	default:
		return nil, smb.StatusNotSupported
	}
}

// defaultFsInfo serializes a synthetic filesystem info struct for the
// requested FS info class. The values are chosen to be uncontroversial: a
// 16TB volume, NTFS-style attributes.
func defaultFsInfo(class byte, label string) ([]byte, uint32) {
	switch class {
	case 0x01: // FileFsVolumeInformation
		// VolumeCreationTime(8) + VolumeSerialNumber(4) + VolumeLabelLength(4)
		// + SupportsObjects(1) + Reserved(1) + VolumeLabel(*)
		labelBytes := unicode.ToUnicode(label)
		buf := make([]byte, 18+len(labelBytes))
		// CreationTime stays zero
		binary.LittleEndian.PutUint32(buf[8:], 0xdeadbeef)
		binary.LittleEndian.PutUint32(buf[12:], uint32(len(labelBytes)))
		// SupportsObjects=0, Reserved=0
		copy(buf[18:], labelBytes)
		return buf, smb.StatusOk

	case 0x03: // FileFsSizeInformation
		// TotalAllocationUnits(8) + AvailableAllocationUnits(8)
		// + SectorsPerAllocationUnit(4) + BytesPerSector(4)
		buf := make([]byte, 24)
		binary.LittleEndian.PutUint64(buf[0:], 1<<24) // 16M units
		binary.LittleEndian.PutUint64(buf[8:], 1<<23) // half free
		binary.LittleEndian.PutUint32(buf[16:], 8)    // 8 sectors/unit
		binary.LittleEndian.PutUint32(buf[20:], 512)  // 512B/sector
		return buf, smb.StatusOk

	case 0x05: // FileFsAttributeInformation
		fsName := unicode.ToUnicode("NTFS")
		// FileSystemAttributes(4) + MaxComponentNameLength(4) + FileSystemNameLength(4) + FileSystemName(*)
		buf := make([]byte, 12+len(fsName))
		// FILE_NAMED_STREAMS is load-bearing for Apple clients — see the
		// constants above. Do not drop it.
		fsAttrs := fsAttrBits()
		binary.LittleEndian.PutUint32(buf[0:], fsAttrs)
		binary.LittleEndian.PutUint32(buf[4:], 255)
		binary.LittleEndian.PutUint32(buf[8:], uint32(len(fsName)))
		copy(buf[12:], fsName)
		return buf, smb.StatusOk

	default:
		return nil, smb.StatusNotSupported
	}
}

// worldReadableSecurityDescriptor returns a self-relative SD granting
// Everyone FILE_ALL_ACCESS through a real one-ACE DACL.
//
// PATCH(selfnas): this used to be a *NULL DACL* SD (no DACL at all, i.e.
// SE_DACL_PRESENT clear). In Windows semantics a null DACL means "allow
// everything", and Windows clients indeed treat it that way — but it says so
// by *absence*, and Apple's client does not extrapolate that far: it parses
// the DACL, finds no ACE, and concludes the account has no write permission
// on the share root. Since the mount's writability is derived from the root
// node, the whole share then shows up as read-only on iOS/macOS even though
// every SMB command succeeds.
//
// So: emit an explicit ACCESS_ALLOWED ACE with FILE_ALL_ACCESS. Same meaning,
// stated positively instead of by omission.
func worldReadableSecurityDescriptor() []byte {
	const fullAccess = 0x001F01FF // FILE_ALL_ACCESS
	// Everyone = S-1-1-0: Revision=1, SubAuthorityCount=1,
	// IdentifierAuthority=1 (SECURITY_WORLD_SID_AUTHORITY), SubAuthority=0.
	everyone := []byte{
		0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00,
	}

	// ACCESS_ALLOWED_ACE: Type(1) Flags(1) Size(2) Mask(4) SID(*)
	ace := make([]byte, 8+len(everyone))
	ace[0] = 0x00 // ACCESS_ALLOWED_ACE_TYPE
	ace[1] = 0x03 // OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE
	binary.LittleEndian.PutUint16(ace[2:], uint16(len(ace)))
	binary.LittleEndian.PutUint32(ace[4:], fullAccess)
	copy(ace[8:], everyone)

	// ACL: Revision(1) Sbz1(1) AclSize(2) AceCount(2) Sbz2(2) + ACEs
	acl := make([]byte, 8+len(ace))
	acl[0] = 0x02 // ACL_REVISION
	binary.LittleEndian.PutUint16(acl[2:], uint16(len(acl)))
	binary.LittleEndian.PutUint16(acl[4:], 1)
	copy(acl[8:], ace)

	// SECURITY_DESCRIPTOR_RELATIVE: Revision(1) Sbz1(1) Control(2)
	// Owner(4) Group(4) Sacl(4) Dacl(4)
	ownerOff := 20
	groupOff := ownerOff + len(everyone)
	daclOff := groupOff + len(everyone)
	out := make([]byte, daclOff+len(acl))
	out[0] = 0x01                                            // Revision
	binary.LittleEndian.PutUint16(out[2:], 0x8000|0x0004)    // SE_SELF_RELATIVE | SE_DACL_PRESENT
	binary.LittleEndian.PutUint32(out[4:], uint32(ownerOff)) // Owner
	binary.LittleEndian.PutUint32(out[8:], uint32(groupOff)) // Group
	binary.LittleEndian.PutUint32(out[16:], uint32(daclOff)) // DACL
	copy(out[ownerOff:], everyone)
	copy(out[groupOff:], everyone)
	copy(out[daclOff:], acl)
	return out
}
