package management

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func tokenPath(root string) string { return filepath.Join(root, "management.token") }

func EnsureToken(root string) error {
	if err := privateRoot(root); err != nil {
		return err
	}
	if _, err := ReadToken(root); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	file, err := os.OpenFile(tokenPath(root), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = file.WriteString(hex.EncodeToString(token[:]))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func ReadToken(root string) (string, error) {
	info, err := os.Lstat(tokenPath(root))
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("management token must be private and regular")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("management token must belong to the current user")
	}
	data, err := os.ReadFile(tokenPath(root))
	if err != nil {
		return "", err
	}
	if len(data) != 64 {
		return "", fmt.Errorf("invalid management token")
	}
	if _, err := hex.DecodeString(string(data)); err != nil {
		return "", err
	}
	return string(data), nil
}

func RequestBody(root string) (json.RawMessage, error) {
	token, err := ReadToken(root)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})
}

func VerifyBody(root string, body json.RawMessage) bool {
	want, err := ReadToken(root)
	if err != nil {
		return false
	}
	var request struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Token) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(request.Token), []byte(want)) == 1
}
