// Package artifact saves user-requested Markdown outputs in a confined directory.
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const MaxMarkdownBytes = 256 << 10

type Saved struct {
	ID       string `json:"artifact_id"`
	Title    string `json:"title"`
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Bytes    int    `json:"bytes"`
}

type Store struct {
	root   *os.Root
	dir    string
	random io.Reader
}

// Open pins the configured output directory. Tool input never supplies a path.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("artifact: output directory is empty")
	}

	// 创建文件 三步走 1 创建绝对录路径 2 创建目录 3 进入目录
	// 获取绝对路径
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("artifact: resolve directory: %w", err)
	}
	// 创建目录
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("artifact: create directory: %w", err)
	}
	// 打开目录
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("artifact: open directory: %w", err)
	}
	return &Store{root: root, dir: abs, random: rand.Reader}, nil
}

func (s *Store) Close() error { return s.root.Close() }

// Save preserves the supplied Markdown verbatim and never overwrites a file.
func (s *Store) Save(ctx context.Context, title, markdown string) (Saved, error) {
	if err := ctx.Err(); err != nil {
		return Saved{}, err
	}
	title = strings.TrimSpace(title)
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > 200 {
		return Saved{}, errors.New("artifact: title must contain 1–200 valid Unicode characters")
	}
	if strings.TrimSpace(markdown) == "" || !utf8.ValidString(markdown) || len(markdown) > MaxMarkdownBytes {
		return Saved{}, errors.New("artifact: Markdown must be nonempty UTF-8 and at most 256 KiB")
	}
	var idBytes [16]byte
	// 存储随机数
	if _, err := io.ReadFull(s.random, idBytes[:]); err != nil {
		return Saved{}, fmt.Errorf("artifact: generate id: %w", err)
	}
	// 随机数生成 id
	id := hex.EncodeToString(idBytes[:])
	name := id + ".md"
	// 打开文件
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Saved{}, fmt.Errorf("artifact: create output (existing files are never overwritten): %w", err)
	}
	// Only remove the exact new file created by this call, never an existing output.
	// 标记是否提交
	committed := false
	// 关闭文件
	defer func() {
		// 兜底关闭
		if !committed {
			_ = f.Close()
			_ = s.root.Remove(name)
		}
	}()
	if _, err := io.WriteString(f, markdown); err != nil {
		return Saved{}, fmt.Errorf("artifact: write output: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Saved{}, err
	}
	// 同步文件  刷新到磁盘
	if err := f.Sync(); err != nil {
		return Saved{}, fmt.Errorf("artifact: flush output: %w", err)
	}
	if err := f.Close(); err != nil {
		return Saved{}, fmt.Errorf("artifact: close output: %w", err)
	}
	committed = true
	return Saved{ID: id, Title: title, Filename: name, Path: filepath.Join(s.dir, name), Bytes: len(markdown)}, nil
}
