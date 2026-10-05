package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/upstream"
)

// npmServer 是一个假的 npm 源，记录收到过哪些路径。
type npmServer struct {
	mu       sync.Mutex
	requests []string
	// serveFile / serveTarball 为 false 时对应路径返回 404，用来验证回退。
	serveFile    bool
	serveTarball bool
	version      string
	// tarball 是预先造好的整包内容（handler 里拿不到 *testing.T）。
	tarball []byte
}

func (s *npmServer) record(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, path)
}

func (s *npmServer) hit(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.requests {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func (s *npmServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version":%q}`, s.version)
		case strings.HasSuffix(r.URL.Path, ".json"):
			if !s.serveFile {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(catalogFixture("cn-model"))
		case strings.HasSuffix(r.URL.Path, ".tgz"):
			if !s.serveTarball {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(s.tarball)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return mux
}

func catalogFixture(id string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"models": []map[string]any{
			{"id": id, "name": strings.ToUpper(id), "credits": "x0.29 credits"},
		},
	})
	return raw
}

// buildTarball 造一个只含一个文件的 .tgz。
func buildTarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("写 tar 头失败: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("写 tar 体失败: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关闭 tar 失败: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关闭 gzip 失败: %v", err)
	}
	return buf.Bytes()
}

// withNPMBases 把三段源列表临时指向测试服务器，结束后还原。
//
// 这三份列表是包级变量，正是为了让「哪些源、什么顺序」可被测试钉住 ——
// 之前 unpkg 漏在列表外、registry 被排在快源前面，都是**静默**的退化：
// 功能还能用，只是每次都白下 55MB。这类问题只有测试能拦住。
func withNPMBases(t *testing.T, base string) {
	t.Helper()
	oldMeta, oldFile, oldTarball := npmMetaBases, npmFileBases, npmTarballBases
	npmMetaBases = []string{base}
	npmFileBases = []string{base}
	npmTarballBases = []string{base}
	t.Cleanup(func() {
		npmMetaBases, npmFileBases, npmTarballBases = oldMeta, oldFile, oldTarball
	})
}

func newNPMClient(t *testing.T, srv *httptest.Server) *upstream.Client {
	t.Helper()
	return &upstream.Client{Control: srv.Client()}
}

// 单文件路径可用时，必须**只**走单文件，绝不能去下整包。
//
// 这条钉住的是本次修复的核心：那份整包实测 55.6MB，官方源 45 秒都下不完，
// 而单文件只有 28KB。一旦有人把 CDN 从列表里拿掉、或把 registry 排到前面，
// 这条会立刻红。
func TestFetchNPMCatalogUsesSingleFileAndSkipsTarball(t *testing.T) {
	srvState := &npmServer{
		serveFile: true, serveTarball: true, version: "9.9.9",
		tarball: buildTarball(t, "package/product.internal.json", catalogFixture("tgz-model")),
	}
	srv := httptest.NewServer(srvState.handler())
	defer srv.Close()
	withNPMBases(t, srv.URL+"/")

	models, err := FetchNPMCatalog(context.Background(), newNPMClient(t, srv), auth.SiteCN)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(models) != 1 || models[0].ID != "cn-model" {
		t.Fatalf("应拿到单文件里的模型，实际 %+v", models)
	}
	if srvState.hit(".tgz") {
		t.Fatal("单文件已成功，却仍然下载了整包 —— 55MB 的白跑又回来了")
	}
}

// 单文件路径不可用时，仍要能回退到整包（兜底能力不能被这次改动弄丢）。
func TestFetchNPMCatalogFallsBackToTarball(t *testing.T) {
	srvState := &npmServer{
		serveFile: false, serveTarball: true, version: "9.9.9",
		tarball: buildTarball(t, "package/product.internal.json", catalogFixture("tgz-model")),
	}
	srv := httptest.NewServer(srvState.handler())
	defer srv.Close()
	withNPMBases(t, srv.URL+"/")

	models, err := FetchNPMCatalog(context.Background(), newNPMClient(t, srv), auth.SiteCN)
	if err != nil {
		t.Fatalf("回退整包失败: %v", err)
	}
	if len(models) != 1 || models[0].ID != "tgz-model" {
		t.Fatalf("应拿到整包里的模型，实际 %+v", models)
	}
}

// 版本查询要能跨源回退：第一个源挂掉不该让整条链路挂掉。
func TestFetchNPMVersionFallsBackAcrossBases(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := &npmServer{version: "1.2.3"}
	goodSrv := httptest.NewServer(good.handler())
	defer goodSrv.Close()

	old := npmMetaBases
	npmMetaBases = []string{bad.URL + "/", goodSrv.URL + "/"}
	t.Cleanup(func() { npmMetaBases = old })

	version, err := fetchNPMVersion(context.Background(), newNPMClient(t, goodSrv))
	if err != nil {
		t.Fatalf("应回退到第二个源，实际报错: %v", err)
	}
	if version != "1.2.3" {
		t.Fatalf("版本号应为 1.2.3，实际 %q", version)
	}
}

// CDN 单文件源必须包含 unpkg，且排在 registry 之前。
//
// 这条防的是「注释说优先走单文件、列表里却没有 unpkg」那个历史 bug 重现。
func TestNPMFileBasesPreferCDN(t *testing.T) {
	if len(npmFileBases) == 0 {
		t.Fatal("单文件源列表为空，会直接退化成下载整包")
	}
	if !strings.Contains(npmFileBases[0], "unpkg.com") {
		t.Fatalf("首个单文件源应是 unpkg，实际 %q", npmFileBases[0])
	}
	for _, base := range npmFileBases {
		if strings.Contains(base, "registry.") {
			t.Fatalf("registry 不接受 `.../@scope/pkg@ver/file` 这种路径（实测 400/422），"+
				"不该出现在单文件源里：%q", base)
		}
	}
}

// 整包兜底的顺序：镜像必须排在官方源前面（实测 14s vs 45s+）。
func TestNPMTarballBasesPreferMirror(t *testing.T) {
	if len(npmTarballBases) == 0 {
		t.Fatal("整包源列表为空，单文件全挂时就没有兜底了")
	}
	if !strings.Contains(npmTarballBases[0], "npmmirror.com") {
		t.Fatalf("首个整包源应是国内镜像，实际 %q", npmTarballBases[0])
	}
}
