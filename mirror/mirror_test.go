package mirror

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestMirror(t *testing.T) {
	t.Parallel()

	c := new(Config)
	_, err := toml.DecodeFile("t/mirror.toml", c)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewMirror(time.Now(), "hogehoge", c); err == nil {
		t.Error(`_, err := NewMirror(time.Now(), "hogehoge", c); err == nil`)
	}

	c2 := *c
	c2.Dir = t.TempDir()
	c2.MaxConns = -1
	if _, err := NewMirror(time.Now(), "ubuntu", &c2); err == nil {
		t.Error("NewMirror must fail with negative max_conns")
	}
	// no snapshot directory should be left.
	if dentries, err := os.ReadDir(c2.Dir); err != nil || len(dentries) != 0 {
		t.Errorf("ReadDir(%s) = %v, %v; want empty", c2.Dir, dentries, err)
	}

	t.Skip()

	m, err := NewMirror(time.Now(), "ubuntu", c)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	err = m.Update(ctx)
	if err != nil {
		t.Error(err)
	}
}
