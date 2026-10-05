package main

// -----------------------------------------------------------------------------
// reset 命令（对照 wb-gateway 的 reset.go）
//
// 清空除登录凭据以外的本地**运行数据**，然后重新拉取模型目录：
//
//	清掉：状态快照（签到状态 / 积分台账 / 任务队列）、模型缓存（含倍率与价格探测结论）、
//	      手工禁用标记（*.disabled）、请求日志归档（request-logs/）、用量统计（wb-stats.json）
//	保留：登录凭据（workbuddy*.json）、config.json、
//	      **session-links/（关联会话登记表）** —— 那是用户自己的关联关系，不是运行态缓存
//
// 为什么要保留 session-links：它记的是「哪几份会话是同一段对话的副本」以及同步基线。
// 删掉它不会让会话消失，但会让所有关联组散掉、同步历史归零 —— 那是用户数据，
// 不属于「清缓存」的范畴（参考项目没有这个功能，所以它的 reset 不涉及这一条）。
//
// 内存里的账号账本由 serve 进程持有，reset 无法直接修改它；
// serve 正在运行时请重启服务，让内存状态与磁盘一起归零。
// -----------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy-gateway/internal/catalog"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

func runReset(args []string) {
	opt, _, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		os.Exit(2)
	}
	cfg, err := config.Load(opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n", err)
		os.Exit(1)
	}
	workDir := cfg.WorkDir

	fmt.Println("================ WorkBuddy 本地数据清理 ================")
	fmt.Printf("工作目录: %s\n", workDir)

	removed := 0
	removeIfExists := func(path, label string) {
		if err := os.Remove(path); err == nil {
			if label != "" {
				fmt.Printf("  已删除%s: %s\n", label, path)
			} else {
				fmt.Printf("  已删除: %s\n", path)
			}
			removed++
		} else if !os.IsNotExist(err) {
			fmt.Printf("  删除失败: %s: %v\n", path, err)
		}
	}

	// 1. 状态快照与模型缓存（含各自的临时文件）。
	for _, name := range []string{
		"wb-models-cache.json",
		"wb-checkin-status.json",
		"wb-credit-history.json",
		"wb-tasks-queue.json",
		"wb-stats.json",
	} {
		base := filepath.Join(workDir, name)
		removeIfExists(base, "")
		removeIfExists(base+".tmp", "")
	}

	// 2. 手工禁用标记（凭据旁边的 `*.disabled`）。
	if entries, err := os.ReadDir(workDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".disabled") || strings.HasSuffix(name, ".json.disabled") {
				removeIfExists(filepath.Join(workDir, name), "失效标记")
			}
		}
	}
	// 显式指定的凭据目录也要扫一遍（标记与凭据同目录）。
	if cfg.AuthDir != "" {
		if entries, err := os.ReadDir(cfg.AuthDir); err == nil {
			for _, e := range entries {
				name := e.Name()
				if strings.HasSuffix(name, ".disabled") || strings.HasSuffix(name, ".json.disabled") {
					removeIfExists(filepath.Join(cfg.AuthDir, name), "失效标记")
				}
			}
		}
	}

	// 3. 请求日志归档（目录整棵删掉，下次启动会自动重建）。
	logsDir := filepath.Join(workDir, "request-logs")
	if entries, err := os.ReadDir(logsDir); err == nil && len(entries) > 0 {
		if err := os.RemoveAll(logsDir); err == nil {
			fmt.Printf("  已删除请求日志归档: %s（%d 个条目）\n", logsDir, len(entries))
			removed++
		}
	}

	// 4. 明确保留的东西（写出来，避免用户以为没清干净）。
	for _, name := range []string{"session-links", "session_links.json", "config.json"} {
		if _, err := os.Stat(filepath.Join(workDir, name)); err == nil {
			fmt.Printf("  已保留（不是缓存）: %s\n", filepath.Join(workDir, name))
		}
	}
	fmt.Println("  已保留: 登录凭据（workbuddy*.json）")

	// 5. 重新拉取模型目录并写入新缓存（与参考实现同序：清完立刻重拉）。
	if err := refreshCatalogAfterReset(cfg); err != nil {
		fmt.Printf("  模型目录刷新失败（不影响清理结果）: %v\n", err)
	} else {
		fmt.Printf("  已重新拉取模型目录: %s\n", filepath.Join(workDir, "wb-models-cache.json"))
	}

	fmt.Printf("清理完成：删除 %d 项。\n", removed)
	fmt.Println("提示：serve 正在运行时，请重启服务让内存里的账号状态与磁盘一起归零。")
}

// refreshCatalogAfterReset 用一份临时账号池拉取模型目录（只读，不改任何账号状态）。
//
// 目录刷新需要凭据（模型列表按站点拉）；没有可用账号时跳过而不是报错 ——
// 「清完缓存但没有账号」是完全正常的状态。
func refreshCatalogAfterReset(cfg *config.Config) error {
	client := &upstream.Client{
		Control:               cfg.Control,
		ChatHTTP:              cfg.Chat,
		IdleTimeout:           cfg.IdleTimeout,
		TransientRetries:      cfg.TransientRetries,
		TransientRetryBackoff: cfg.TransientRetryBackoff,
		Verbose:               cfg.Verbose,
		Logf:                  log.Printf,
	}
	accountPool := pool.New(cfg, client)
	if _, errs := accountPool.Load(); len(errs) > 0 {
		// 部分凭据无效不该阻断清理：能拉到目录就拉，拉不到在下面统一提示。
		log.Printf("[凭据] 跳过无效文件 %d 个", len(errs))
	}
	if len(accountPool.Snapshot()) == 0 {
		return fmt.Errorf("没有可用账号，跳过")
	}
	cat := catalog.New(filepath.Join(cfg.WorkDir, "wb-models-cache.json"), client, accountPool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return cat.RefreshOnce(ctx)
}
