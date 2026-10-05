// Package accountmeta 保存账号的**本地展示元数据**：备注与卡片显示字段。
//
// 为什么单独一个包、单独一个文件：这两项只属于本机面板的展示偏好，
// 不属于凭据（写进凭据文件会被别的工具当成未知字段，也可能随导出泄露），
// 也不属于账号池的治理状态（冷却、熔断那些）。
//
// 存储：工作目录下的单文件 JSON（`wb-account-meta.json`），
// 结构 `{"version":1,"accounts":{"<accountID>":{"note":"..","display_field":"note"}}}`，
// 原子替换写入（tmp + rename），所有方法对并发安全、对 nil 接收者安全。
//
// 与 wb-switch 的 account-info-dialog 对齐：
//   - 备注上限 24 个字符（按**字符**数而不是字节数，中文 24 字 == 24 字符）；
//   - 显示字段取值 nickname / phone / note；本项目没有手机号数据源，
//     phone 仍允许存储（契约对齐），由前端在无数据时禁用并回退昵称。
package accountmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// MaxNoteRunes 是备注长度上限（与 wb-switch 的 NOTE_MAX_CHARS 一致）。
const MaxNoteRunes = 24

// 显示字段取值。空串表示「未设置」，由前端按昵称显示。
const (
	DisplayNickname = "nickname"
	DisplayPhone    = "phone"
	DisplayNote     = "note"
)

var (
	// ErrNoteTooLong 备注超过 MaxNoteRunes 个字符（调用方应映射成 400）。
	ErrNoteTooLong = errors.New("备注不能超过 24 个字符")
	// ErrInvalidDisplayField 显示字段不在 nickname / phone / note 之内（调用方应映射成 400）。
	ErrInvalidDisplayField = errors.New("显示字段只能是 nickname、phone 或 note")
)

// Entry 是单个账号的元数据。
type Entry struct {
	Note string `json:"note,omitempty"`
	// DisplayField 空串 = 未设置（前端回退昵称）。
	DisplayField string `json:"display_field,omitempty"`
}

// fileDoc 是落盘结构。
type fileDoc struct {
	Version  int              `json:"version"`
	Accounts map[string]Entry `json:"accounts"`
}

// Store 是元数据的读写入口。零值不可用；用 New 构造。
// 所有方法对 nil 接收者安全（未接线时静默跳过/返回空，调用方不必判空）。
type Store struct {
	mu     sync.Mutex
	path   string
	data   fileDoc
	loaded bool
}

// New 创建元数据存储。path 通常是 `<工作目录>/wb-account-meta.json`。
func New(path string) *Store { return &Store{path: path} }

func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.data = fileDoc{Version: 1, Accounts: map[string]Entry{}}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // 文件不存在是正常路径
	}
	var parsed fileDoc
	if json.Unmarshal(raw, &parsed) != nil {
		return // 损坏时按空处理，不影响账号页
	}
	if parsed.Accounts == nil {
		parsed.Accounts = map[string]Entry{}
	}
	if parsed.Version == 0 {
		parsed.Version = 1
	}
	s.data = parsed
}

// saveLocked 原子写盘（tmp + rename）。写失败只记录到返回值，调用方决定怎么处理。
func (s *Store) saveLocked() error {
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

// Get 返回某账号的元数据；无记录时返回零值（Note/DisplayField 均为空串）。
func (s *Store) Get(accountID string) Entry {
	if s == nil {
		return Entry{}
	}
	accountID = strings.TrimSpace(accountID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.data.Accounts[accountID]
}

// All 返回全部账号元数据的副本（键是账号 ID）。
func (s *Store) All() map[string]Entry {
	if s == nil {
		return map[string]Entry{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make(map[string]Entry, len(s.data.Accounts))
	for k, v := range s.data.Accounts {
		out[k] = v
	}
	return out
}

// Update 更新某账号的备注 / 显示字段并落盘，返回更新后的条目。
//
// 语义（与 PATCH 契约一致）：
//   - note == nil 表示本次不改备注；note 非 nil 时按 TrimSpace 后存储，
//     空串表示**清除备注**；
//   - displayField == nil 表示不改；非 nil 时必须是 nickname / phone / note
//     之一，空串表示清除（回退昵称）；
//   - note 与 displayField 都清空后，该账号的条目整体删除（文件保持整洁）。
//
// 校验失败返回 ErrNoteTooLong / ErrInvalidDisplayField，不修改任何状态。
func (s *Store) Update(accountID string, note *string, displayField *string) (Entry, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return Entry{}, errors.New("账号 ID 不能为空")
	}

	// 先校验后落锁：校验不依赖存储状态，放在锁外让错误路径不碰 IO。
	var trimmedNote *string
	if note != nil {
		v := strings.TrimSpace(*note)
		if utf8.RuneCountInString(v) > MaxNoteRunes {
			return Entry{}, ErrNoteTooLong
		}
		trimmedNote = &v
	}
	var field *string
	if displayField != nil {
		v := strings.TrimSpace(*displayField)
		if v != "" && v != DisplayNickname && v != DisplayPhone && v != DisplayNote {
			return Entry{}, ErrInvalidDisplayField
		}
		field = &v
	}

	if s == nil {
		// 未接线时不持久化，但保持「校验通过即成功」的语义。
		var entry Entry
		if trimmedNote != nil {
			entry.Note = *trimmedNote
		}
		if field != nil {
			entry.DisplayField = *field
		}
		return entry, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	entry := s.data.Accounts[accountID]
	if trimmedNote != nil {
		entry.Note = *trimmedNote
	}
	if field != nil {
		entry.DisplayField = *field
	}

	if entry.Note == "" && entry.DisplayField == "" {
		delete(s.data.Accounts, accountID)
	} else {
		s.data.Accounts[accountID] = entry
	}
	if err := s.saveLocked(); err != nil {
		return Entry{}, fmt.Errorf("账号元数据落盘失败: %w", err)
	}
	return entry, nil
}
