package catalog

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/upstream"
)

// -----------------------------------------------------------------------------
// npm 静态目录兜底
//
// 官方把各站点模型目录放在 npm 包 @tencent-ai/codebuddy-code 的两个文件里：
//
//	cn   → product.internal.json
//	intl → product.cloudhosted.json
//
// 实时接口失败时用它兜底，保证 /v1/models 仍有内容（模型本身是透传的，
// 列表只影响客户端自动补全，不影响调用）。
// -----------------------------------------------------------------------------

const npmPackage = "@tencent-ai/codebuddy-code"

// npmCatalogFiles 是各站点对应的包内文件名。
var npmCatalogFiles = map[string]string{
	auth.SiteCN:   "product.internal.json",
	auth.SiteINTL: "product.cloudhosted.json",
}

// npmMetaBases 查版本号用。只有 registry 提供 `/latest`，CDN 没有这个端点。
var npmMetaBases = []string{
	"https://registry.npmmirror.com/",
	"https://registry.npmjs.org/",
}

// npmFileBases 直接取包内单个文件用。
//
// 只有 unpkg 这类 CDN 支持 `.../@scope/pkg@ver/<file>` 这种地址形态
// （实测 1.7s / 28KB）。原先这份列表里放的是两个 registry，而 registry 不接受
// 该路径 —— 实测分别返回 400 / 422。也就是说上面注释里写的
// 「优先走 unpkg 单文件（省一次解包）」**从来没有生效过**：
// 每次都白跑两趟，然后退回下载 55MB 的整包。
var npmFileBases = []string{
	"https://unpkg.com/",
}

// npmTarballBases 整包兜底，只在单文件路径全挂时才走。
//
// 顺序与原先相反（原来官方源在前）：这份包实测 55.6MB，
// registry.npmjs.org 下 45 秒都下不完，而 registry.npmmirror.com 只要 14 秒。
// 原来那句「先官方源、后国内镜像（镜像更快且不被墙）」本身就自相矛盾 ——
// 把更快的那个放在后面，等于让它永远轮不上。
var npmTarballBases = []string{
	"https://registry.npmmirror.com/",
	"https://registry.npmjs.org/",
}

// npmMetaTimeout / npmTarballTimeout 是**单次尝试**的超时。
//
// 为什么必须单独设而不是共用调用方的 ctx：这几段请求原先共用启动时的 30s bootCtx，
// 于是**第一个**源会把预算全部吃光，后面真正能用的源连试的机会都没有 ——
// 而且最后的报错落在能用的那个源上，把「官方源慢」误读成「镜像挂了」。
//
// npmTarballTimeout 取 55s 而不是更大：控制面 HTTP 客户端自身有 60s 总超时
// （config.DefaultControlTimeout），设得更长只会让客户端先超时，
// 报出来的错误就不是我们这条上下文了。
const (
	npmMetaTimeout    = 8 * time.Second
	npmTarballTimeout = 55 * time.Second
)

// npmMaxBytes 限制 tgz 读取量，防止异常大包拖垮内存。
const npmMaxBytes = 80 << 20

// FetchNPMCatalog 拉取某站点的 npm 静态目录。
func FetchNPMCatalog(ctx context.Context, client *upstream.Client, site string) ([]Model, error) {
	file, ok := npmCatalogFiles[site]
	if !ok {
		return nil, fmt.Errorf("站点 %s 无 npm 目录文件映射", site)
	}
	return fetchNPMFile(ctx, client, file)
}

// npmCommonFile 是各站点的**公共基座**目录。
//
// 上游对部分站点下发的是「覆盖层」而非完整目录：响应里带
// `include: ["../common/product.json"]`。这个 "../common/product.json" 指的是
// 客户端资源目录下的公共基座，npm 包把它平铺在包根，即 product.json。
// （上游 HTTP 端点不提供该文件：实测 /common/product.json 等四个候选路径全 404，
// 所以只能从 npm 包取。）
const npmCommonFile = "product.json"

// FetchNPMCommonCatalog 拉取公共基座目录（供声明了 include 的站点合并）。
func FetchNPMCommonCatalog(ctx context.Context, client *upstream.Client) ([]Model, error) {
	return fetchNPMFile(ctx, client, npmCommonFile)
}

// fetchNPMFile 取 npm 包内某个目录文件：先 CDN 单文件，全挂再回退整包。
func fetchNPMFile(ctx context.Context, client *upstream.Client, file string) ([]Model, error) {
	version, err := fetchNPMVersion(ctx, client)
	if err != nil {
		return nil, err
	}

	var lastErr error
	// 先走 CDN 单文件（KB 级），全挂再回退整包（55MB 级）。
	for _, base := range npmFileBases {
		url := base + npmPackage + "@" + version + "/" + file
		actx, cancel := context.WithTimeout(ctx, npmMetaTimeout)
		models, err := fetchNPMJSON(actx, client, url)
		cancel()
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	for _, base := range npmTarballBases {
		url := base + npmPackage + "/-/codebuddy-code-" + version + ".tgz"
		actx, cancel := context.WithTimeout(ctx, npmTarballTimeout)
		models, err := fetchNPMTarball(actx, client, url, file)
		cancel()
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// fetchNPMVersion 查最新版本号（逐个源单独计时）。
func fetchNPMVersion(ctx context.Context, client *upstream.Client) (string, error) {
	var lastErr error
	for _, base := range npmMetaBases {
		actx, cancel := context.WithTimeout(ctx, npmMetaTimeout)
		version, err := fetchNPMVersionFrom(actx, client, base)
		cancel()
		if err == nil {
			return version, nil
		}
		lastErr = err
	}
	return "", lastErr
}

// fetchNPMVersionFrom 从单个源查最新版本号。
func fetchNPMVersionFrom(ctx context.Context, client *upstream.Client, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+npmPackage+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Control.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || strings.TrimSpace(doc.Version) == "" {
		return "", errors.New("manifest 缺少 version")
	}
	return strings.TrimSpace(doc.Version), nil
}

// fetchNPMJSON 直接取包内 JSON 文件（unpkg 形态）。
func fetchNPMJSON(ctx context.Context, client *upstream.Client, url string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Control.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return parseNPMCatalog(body)
}

// fetchNPMTarball 下载 tgz 并以流式方式只取出目标文件（不落盘、不整包驻留内存）。
// 用标准库 archive/tar + compress/gzip，比手工解析包结构可靠得多。
func fetchNPMTarball(ctx context.Context, client *upstream.Client, url, want string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Control.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(io.LimitReader(resp.Body, npmMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("解压 tgz 失败: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 tgz 失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// 包内路径形如 package/product.internal.json，按文件名匹配即可
		if path.Base(hdr.Name) != want {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, 16<<20))
		if err != nil {
			return nil, fmt.Errorf("读取包内文件失败: %w", err)
		}
		return parseNPMCatalog(body)
	}
	return nil, fmt.Errorf("包内未找到 %s", want)
}

// parseNPMCatalog 解析 npm 目录文件（结构与实时接口的 models 数组一致）。
func parseNPMCatalog(data []byte) ([]Model, error) {
	var doc struct {
		Models *[]struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Credits any    `json:"credits"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 npm 目录失败: %w", err)
	}
	if doc.Models == nil {
		return nil, errors.New("npm 目录缺少 models 数组")
	}
	seen := map[string]bool{}
	out := make([]Model, 0, len(*doc.Models))
	for _, m := range *doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		entry := Model{ID: id, Name: strings.TrimSpace(m.Name)}
		if s, ok := m.Credits.(string); ok {
			entry.Credits = strings.TrimSpace(s)
		}
		if v, ok := parseCredits(m.Credits); ok {
			entry.BaseMultiplier = v
			entry.Multiplier = v
			entry.HasMultiplier = true
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, errors.New("npm 目录为空")
	}
	return out, nil
}

// parseCredits 把 credits 字段解析为数值倍率，例如 "x0.29 credits" → 0.29。
func parseCredits(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		s = strings.TrimPrefix(s, "x")
		s = strings.ReplaceAll(s, "credits", "")
		s = strings.ReplaceAll(s, "credit", "")
		s = strings.TrimSpace(s)
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}
