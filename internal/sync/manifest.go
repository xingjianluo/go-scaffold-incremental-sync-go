package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Manifest 记录上次生成时各文件的哈希，作为三方合并的 base 侧。
type Manifest struct {
	Files map[string]string // path -> 上次生成内容的 SHA-256（hex）
}

// ManifestPath 是清单在项目目录中的固定位置。
const ManifestPath = ".scaffold-manifest.json"

// Hash 计算内容的确定性哈希。
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// LoadManifest 读取项目目录中的清单；不存在时返回空清单。
func LoadManifest(projectDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(projectDir, ManifestPath))
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Files: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, nil
}

// Save 将清单写盘。encoding/json 对 map 键按字典序输出，
// 且清单不含时间戳，因此相同内容必然产生相同的字节。
func (m *Manifest) Save(projectDir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(projectDir, ManifestPath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // 清理临时文件，避免遗留未释放资源
		return fmt.Errorf("commit manifest: %w", err)
	}
	return nil
}
