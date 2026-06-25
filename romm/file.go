package romm

import (
	"os"
	"path/filepath"
)

// readFile reads savePath and returns its bytes plus its basename (used as the
// multipart filename for save uploads).
func readFile(savePath string) (data []byte, base string, err error) {
	data, err = os.ReadFile(savePath)
	if err != nil {
		return nil, "", err
	}
	return data, filepath.Base(savePath), nil
}
