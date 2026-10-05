package localapps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// 备份 / 恢复
//
// 为什么它是**所有写入路径的强制前置**：这些文件是别的程序的登录态。
// 写坏了用户的后果不是「本工具少个功能」，而是「客户端登录失效、要重新扫码」，
// 而用户根本不会想到是网关干的。所以：
//
//  1. 写之前必须备份（BackupFor）；
//  2. 写之后必须回读校验（各 write* 内部做）；
//  3. 失败必须自动回滚（RestoreBackup）；
//  4. 备份有清单，用户能自己看、能手动撤。
//
// 备份放在**本工具自己的数据目录**下，不与目标应用混在一起 ——
// 把备份塞进 `~/.codebuddy/` 会让那个目录多出 CLI 不认识的文件。
// -----------------------------------------------------------------------------

const backupsDirName = "local-app-backups"

// BackupEntry 是一次备份的记录。
type BackupEntry struct {
	// ID 是备份标识（也是目录名），形如 `<target>-<时间戳>`。
	ID     string   `json:"id"`
	Target TargetID `json:"target"`
	// Files 是被备份的原始文件路径（相对信息不存，存绝对路径便于恢复）。
	Files []BackupFile `json:"files"`
	// CreatedAt 是 Unix 秒。
	CreatedAt int64  `json:"created_at"`
	Reason    string `json:"reason"`
}

// BackupFile 是备份里的一个文件。
type BackupFile struct {
	// Original 是原始绝对路径。
	Original string `json:"original"`
	// Name 是备份目录下的文件名（用序号前缀避免同名冲突）。
	Name string `json:"name"`
	// Size 是原始大小，便于用户判断备份是否完整。
	Size int64 `json:"size"`
	// Existed 为假表示备份时该文件**还不存在**（写入会新建它）。
	// 恢复时要删除而不是写回空内容 —— 否则会在本不存在的路径留下一个 0 字节文件，
	// 让目标应用读到「空的登录态」，比没有更糟。
	Existed bool `json:"existed"`
}

// backupRoot 返回备份根目录。
func backupRoot() string {
	return filepath.Join(localAppData(), "wb-gateway", backupsDirName)
}

// BackupFor 备份这些路径，返回备份记录。
//
// 路径不存在不会报错，而是记为 Existed=false —— 首次配置时目标文件本来就可能不存在。
func BackupFor(target TargetID, paths []string, reason string) (*BackupEntry, error) {
	now := time.Now()
	id := fmt.Sprintf("%s-%s", target, now.Format("20060102-150405"))
	dir := filepath.Join(backupRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建备份目录失败: %w", err)
	}

	entry := &BackupEntry{
		ID:        id,
		Target:    target,
		CreatedAt: now.Unix(),
		Reason:    reason,
	}
	for i, p := range paths {
		bf := BackupFile{Original: p, Name: fmt.Sprintf("%02d-%s", i, filepath.Base(p))}
		data, err := os.ReadFile(p)
		if err != nil {
			// 读不到就标记为「原本不存在」，继续处理其余文件 ——
			// 一个目标通常涉及多个文件，不能因为缺一个就整个放弃备份。
			bf.Existed = false
		} else {
			bf.Existed = true
			bf.Size = int64(len(data))
			if err := os.WriteFile(filepath.Join(dir, bf.Name), data, 0o600); err != nil {
				return nil, fmt.Errorf("写入备份文件失败: %w", err)
			}
		}
		entry.Files = append(entry.Files, bf)
	}

	if err := writeManifest(dir, entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// RestoreBackup 按备份记录恢复。
//
// 返回逐文件的处理结果（成功/失败），**不做「全成功才算成功」的短路** ——
// 多文件目标里某一个恢复失败时，用户更需要知道「哪个文件没回来」，
// 而不是一句笼统的失败。
func RestoreBackup(id string) ([]string, error) {
	entry, err := loadManifest(id)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(backupRoot(), entry.ID)

	notes := []string{}
	for _, bf := range entry.Files {
		if !bf.Existed {
			// 备份时不存在 → 恢复到「不存在」这个状态。
			if err := os.Remove(bf.Original); err != nil && !os.IsNotExist(err) {
				notes = append(notes, fmt.Sprintf("%s：删除失败 %v", bf.Original, err))
				continue
			}
			notes = append(notes, fmt.Sprintf("%s：已按备份时的状态删除", filepath.Base(bf.Original)))
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, bf.Name))
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s：读取备份失败 %v", bf.Original, err))
			continue
		}
		if err := atomicWrite(bf.Original, data); err != nil {
			notes = append(notes, fmt.Sprintf("%s：写入失败 %v", bf.Original, err))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s：已恢复到备份时的内容（%d 字节）",
			filepath.Base(bf.Original), len(data)))
	}
	return notes, nil
}

// ListBackups 列出备份，最近的在前。
func ListBackups(limit int) ([]BackupEntry, error) {
	root := backupRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupEntry{}, nil
		}
		return nil, err
	}
	out := make([]BackupEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		entry, err := loadManifest(e.Name())
		if err != nil {
			continue // 清单坏了就跳过，不因为一条坏记录让整个列表读不出来
		}
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func manifestPath(id string) string {
	return filepath.Join(backupRoot(), id, "manifest.json")
}

func writeManifest(dir string, entry *BackupEntry) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o600)
}

func loadManifest(id string) (*BackupEntry, error) {
	// 防路径穿越：备份 ID 只允许出现文件名安全字符。
	if strings.ContainsAny(id, `/\`) || id == "" || strings.Contains(id, "..") {
		return nil, fmt.Errorf("非法的备份标识 %q", id)
	}
	raw, err := os.ReadFile(manifestPath(id))
	if err != nil {
		return nil, fmt.Errorf("读取备份清单失败: %w", err)
	}
	var entry BackupEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("备份清单不是有效 JSON: %w", err)
	}
	return &entry, nil
}

// atomicWrite 原子写入（同目录 tmp + rename），权限 0600。
//
// 为什么必须原子：这些文件被别的程序读。直接 `os.WriteFile` 会在写入过程中
// 留下一个被截断的文件，此刻目标应用若正好读取，看到的就是**残缺的登录态**
// —— 而这类问题几乎不可能被归因到我们身上。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
