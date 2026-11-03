package splitter

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2/hclwrite"
)

func newDirectory(path string) error {
	if path == "" {
		return fmt.Errorf("output directory is required")
	}
	return os.Mkdir(path, 0700)
}

func writePrivate(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func writeHCL(path, source string) error {
	return writePrivate(path, hclwrite.Format([]byte(source)))
}
