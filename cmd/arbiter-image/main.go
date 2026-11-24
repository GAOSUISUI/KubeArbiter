// Command arbiter-image 离线拼装单层 OCI 镜像归档。
//
// 集群节点只有 containerd，没有 docker/nerdctl/buildkit，也不需要联网；
// 这里把交叉编译好的静态二进制（可选附带 CA 证书）打成一个 OCI layout tar，
// 节点上 `ctr -n k8s.io images import` 即可使用。等价于旧版
// deploy/arbiter/build-image.sh，但不再依赖 python3，可在 Windows 构建机直接运行。
//
// 用法：
//
//	go run ./cmd/arbiter-image -bin bin/arbiter-api-linux \
//	    -ref docker.io/kubearbiter/arbiter-api:0.9.3 -out bin/arbiter-image-0.9.3.tar
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeConfig   = "application/vnd.oci.image.config.v1+json"
	mediaTypeLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type imageConfig struct {
	Architecture string        `json:"architecture"`
	OS           string        `json:"os"`
	Created      string        `json:"created"`
	Config       runtimeConfig `json:"config"`
	RootFS       rootFS        `json:"rootfs"`
	History      []history     `json:"history"`
}

type runtimeConfig struct {
	User         string              `json:"User"`
	Entrypoint   []string            `json:"Entrypoint"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
}

type rootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type history struct {
	Created   string `json:"created"`
	CreatedBy string `json:"created_by"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	Manifests     []descriptor `json:"manifests"`
}

func main() {
	binPath := flag.String("bin", "", "linux/amd64 静态二进制路径")
	ref := flag.String("ref", "", "镜像引用，如 docker.io/kubearbiter/arbiter-api:0.9.3")
	outPath := flag.String("out", "", "输出 OCI 归档 tar 路径")
	caPath := flag.String("ca", "/etc/ssl/certs/ca-certificates.crt", "CA 证书路径，不存在则跳过")
	created := flag.String("created", time.Now().UTC().Format(time.RFC3339), "镜像创建时间（RFC3339）")
	flag.Parse()

	if *binPath == "" || *ref == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "用法: arbiter-image -bin <二进制> -ref <镜像引用> -out <输出.tar>")
		flag.PrintDefaults()
		os.Exit(2)
	}
	if err := run(*binPath, *ref, *outPath, *caPath, *created); err != nil {
		fmt.Fprintln(os.Stderr, "构建失败:", err)
		os.Exit(1)
	}
}

func run(binPath, ref, outPath, caPath, created string) error {
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return fmt.Errorf("读取二进制: %w", err)
	}

	layerRaw, err := buildLayer(binData, caPath)
	if err != nil {
		return err
	}
	layerGz, err := gzipBytes(layerRaw)
	if err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "arbiter-image-")
	if err != nil {
		return fmt.Errorf("创建临时目录: %w", err)
	}
	defer os.RemoveAll(workDir)

	// 单层：静态二进制 + CA 证书（Go 静态二进制不依赖 libc）。
	diffID, _, err := writeBlob(workDir, layerRaw)
	if err != nil {
		return err
	}
	layerDigest, layerSize, err := writeBlob(workDir, layerGz)
	if err != nil {
		return err
	}

	cfg := imageConfig{
		Architecture: "amd64",
		OS:           "linux",
		Created:      created,
		Config: runtimeConfig{
			User:       "65532:65532",
			Entrypoint: []string{"/usr/local/bin/arbiter-api"},
			ExposedPorts: map[string]struct{}{
				"8080/tcp": {},
				"9443/tcp": {},
			},
		},
		RootFS:  rootFS{Type: "layers", DiffIDs: []string{diffID}},
		History: []history{{Created: created, CreatedBy: "kubearbiter offline image assembly"}},
	}
	cfgData, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化镜像配置: %w", err)
	}
	cfgDigest, cfgSize, err := writeBlob(workDir, cfgData)
	if err != nil {
		return err
	}

	manData, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		Config:        descriptor{MediaType: mediaTypeConfig, Digest: cfgDigest, Size: cfgSize},
		Layers:        []descriptor{{MediaType: mediaTypeLayer, Digest: layerDigest, Size: layerSize}},
	})
	if err != nil {
		return fmt.Errorf("序列化 manifest: %w", err)
	}
	manDigest, manSize, err := writeBlob(workDir, manData)
	if err != nil {
		return err
	}

	idxData, err := json.Marshal(index{
		SchemaVersion: 2,
		Manifests: []descriptor{{
			MediaType:   mediaTypeManifest,
			Digest:      manDigest,
			Size:        manSize,
			Annotations: map[string]string{"org.opencontainers.image.ref.name": ref},
		}},
	})
	if err != nil {
		return fmt.Errorf("序列化 index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "index.json"), idxData, 0o644); err != nil {
		return fmt.Errorf("写入 index.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return fmt.Errorf("写入 oci-layout: %w", err)
	}

	if err := writeArchive(workDir, outPath); err != nil {
		return err
	}
	fmt.Printf("镜像归档: %s (%s)\n", outPath, ref)
	fmt.Printf("manifest: %s\n", manDigest)
	return nil
}

// buildLayer 生成未压缩的 layer tar：/usr/local/bin/arbiter-api，存在 CA 证书时一并放入。
func buildLayer(binData []byte, caPath string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := addFile(tw, "usr/local/bin/arbiter-api", binData, 0o755); err != nil {
		return nil, err
	}
	// Windows 构建机上通常没有该路径，缺少时只提示不失败：集群内 API 调用使用自带 CA。
	if caData, err := os.ReadFile(caPath); err == nil {
		if err := addFile(tw, "etc/ssl/certs/ca-certificates.crt", caData, 0o644); err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintf(os.Stderr, "提示: 未找到 CA 证书 %s，镜像将不包含系统根证书\n", caPath)
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("关闭 layer tar: %w", err)
	}
	return buf.Bytes(), nil
}

func addFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		Uid:      0,
		Gid:      0,
		Uname:    "root",
		Gname:    "root",
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("写入 layer 头 %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("写入 layer 内容 %s: %w", name, err)
	}
	return nil
}

// gzipBytes 固定 mtime，保证同一二进制重复构建得到相同 digest。
func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("初始化 gzip: %w", err)
	}
	zw.Header.ModTime = time.Unix(0, 0)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("压缩 layer: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("关闭 gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// writeBlob 按 OCI layout 把内容写入 blobs/sha256/<digest>，返回 digest 与大小。
func writeBlob(workDir string, data []byte) (string, int64, error) {
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	dir := filepath.Join(workDir, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("创建 blobs 目录: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")), data, 0o644); err != nil {
		return "", 0, fmt.Errorf("写入 blob %s: %w", digest, err)
	}
	return digest, int64(len(data)), nil
}

// writeArchive 把 OCI layout 目录打成 tar。
func writeArchive(workDir, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("创建输出文件: %w", err)
	}

	tw := tar.NewWriter(f)
	walkErr := filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(workDir, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, src)
		src.Close()
		return copyErr
	})
	if walkErr != nil {
		return fmt.Errorf("打包 OCI 归档: %w", walkErr)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("关闭归档: %w", err)
	}
	return f.Close()
}
