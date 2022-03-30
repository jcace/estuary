package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/facebookgo/atomicfile"
)

var ErrNotInitialized = errors.New("node not initialized, please run configure")

type Config struct {
	ListenAddrs   []string `json:"Swarm"`
	AnnounceAddrs []string `json:"Announce"`

	Blockstore string

	WriteLog          string
	HardFlushWriteLog bool
	WriteLogTruncate  bool
	NoBlockstoreCache bool
	NoLimiter         bool

	Libp2pKeyFile string

	Datastore string

	WalletDir string

	BitswapConfig BitswapConfig
}

type BitswapConfig struct {
	MaxOutstandingBytesPerPeer int64
	TargetMessageSize          int
}

func (cfg *Config) AddListener(newAddr string) {
	for _, addr := range cfg.ListenAddrs {
		if addr == newAddr {
			return
		}
	}
	cfg.ListenAddrs = append(cfg.ListenAddrs, newAddr)
}

// encode configuration with JSON
func encode(cfg interface{}, w io.Writer) error {
	// need to prettyprint, hence MarshalIndent, instead of Encoder
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

func load(cfg interface{}, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			err = ErrNotInitialized
		}
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(cfg); err != nil {
		return fmt.Errorf("failure to decode config: %s", err)
	}
	return nil
}

// save writes the config from `cfg` into `filename`.
func save(cfg interface{}, filename string) error {
	err := os.MkdirAll(filepath.Dir(filename), 0755)
	if err != nil {
		return err
	}

	f, err := atomicfile.New(filename, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	return encode(cfg, f)
}

var ErrEmptyPath = errors.New("node not initialized, please run configure")

func MakeAbsolute(root string, path string) (error, string) {
	switch {
	case path == "":
		return ErrEmptyPath, ""
	case filepath.IsAbs(path):
		return nil, path
	default:
		return nil, filepath.Join(root, path)
	}
}

func MakeAbsoluteDefault(root string, path string, dflt string) string {
	switch {
	case path == "":
		_, result := MakeAbsolute(root, dflt)
		return result // ignroe error; if dflt is empty, result is empty
	default:
		_, result := MakeAbsolute(root, path)
		return result
	}
}
