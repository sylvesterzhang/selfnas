package main

import (
	"context"
	"sync"

	dcerpcserver "github.com/jfjallid/go-smb/dcerpc/server"
	"github.com/jfjallid/go-smb/smb"
)

// 0xC00000D9 = STATUS_PIPE_EMPTY：管道里暂时没有数据
const statusPipeEmpty uint32 = 0xc00000d9

// pipeAdapter 把 dcerpc 的 PipeHandler 包成「既能 Transceive、也能 Write/Read」的管道后端。
//
// 为什么必须有它：Windows / macOS 走 FSCTL_PIPE_TRANSCEIVE 一问一答，
// 而 iOS「文件」App 是分开的 —— 先 WRITE 把 Bind/Request PDU 写进命名管道，
// 再 READ 读回响应。库自带的 PipeHandler 只认 Transceive，对 Write 直接回
// STATUS_NOT_SUPPORTED(0xc00000bb)，iOS 于是报「无法完成此操作，因为发生意外错误」。
// 2026-10-29 实测确认。
type pipeAdapter struct {
	h *dcerpcserver.PipeHandler

	mu  sync.Mutex
	buf []byte // 等待客户端 READ 取走的响应字节
}

func (p *pipeAdapter) Transceive(_ context.Context, in []byte) ([]byte, uint32, error) {
	return p.h.Transceive(context.Background(), in)
}

func (p *pipeAdapter) Write(_ context.Context, data []byte) (int, uint32, error) {
	out, status, err := p.h.Transceive(context.Background(), data)
	if err != nil {
		return 0, status, err
	}
	if status != smb.StatusOk {
		return 0, status, nil
	}
	p.mu.Lock()
	p.buf = append(p.buf, out...)
	p.mu.Unlock()
	return len(data), smb.StatusOk, nil
}

func (p *pipeAdapter) Read(_ context.Context, max int) ([]byte, uint32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) == 0 {
		return nil, statusPipeEmpty, nil
	}
	if max <= 0 || max > len(p.buf) {
		max = len(p.buf)
	}
	out := make([]byte, max)
	copy(out, p.buf[:max])
	p.buf = p.buf[max:]
	return out, smb.StatusOk, nil
}

func (p *pipeAdapter) Close(_ context.Context) error {
	p.mu.Lock()
	p.buf = nil
	p.mu.Unlock()
	return nil
}
