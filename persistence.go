package flagengine

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// MarshalConfig 将配置序列化为规范 JSON（缩进、键序由 struct 决定）。
// 同一配置的序列化结果稳定，便于存盘、比对与审计。
func MarshalConfig(cfg *Config) ([]byte, error) {
	return json.MarshalIndent(cfg, "", "  ")
}

// UnmarshalConfig 从 JSON 反序列化配置，不做语义校验
// （交给 Engine.New / Commit，保证载入与提交走同一条校验路径）。
func UnmarshalConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfigFile 将配置原子写入文件：先写同目录临时文件再 rename，
// 避免进程崩溃留下半个配置文件。
func SaveConfigFile(path string, cfg *Config) error {
	data, err := MarshalConfig(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ffconfig-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后 Remove 一个不存在的路径，无副作用
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LoadConfigFile 读取并解析配置文件。
func LoadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return UnmarshalConfig(data)
}

// NewFromFile 是便捷构造器：从文件载入配置并创建引擎。
// 这样进程重启后载入同一文件，同一输入的评估结果与重启前完全一致。
func NewFromFile(path string, opts Options) (*Engine, error) {
	cfg, err := LoadConfigFile(path)
	if err != nil {
		return nil, err
	}
	return New(opts, cfg)
}
