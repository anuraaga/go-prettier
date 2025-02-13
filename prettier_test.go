package prettier

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/wasilibs/go-prettier/internal/runner"
)

//go:embed testdata/in
var testFiles embed.FS

//go:embed testdata/out
var outFiles embed.FS

//go:embed testdata/outtabwidth4
var outFilesTabWidth4 embed.FS

func TestRun(t *testing.T) {
	t.Parallel()

	testFiles, _ := fs.Sub(testFiles, "testdata/in")
	outFiles, _ := fs.Sub(outFiles, "testdata/out")
	outFilesTabWidth4, _ := fs.Sub(outFilesTabWidth4, "testdata/outtabwidth4")

	tests := []struct {
		name  string
		args  runner.RunArgs
		outFS fs.FS
	}{
		{
			name: "no config, write",
			args: runner.RunArgs{
				Write: true,
			},
			outFS: outFiles,
		},
		{
			name: "json config, write",
			args: runner.RunArgs{
				Write:  true,
				Config: filepath.Join("testdata", ".prettierrc"),
			},
			outFS: outFilesTabWidth4,
		},
		{
			name: "yaml config, write",
			args: runner.RunArgs{
				Write:  true,
				Config: filepath.Join("testdata", "prettierrc.yaml"),
			},
			outFS: outFilesTabWidth4,
		},
		{
			name: "toml config, write",
			args: runner.RunArgs{
				Write:  true,
				Config: filepath.Join("testdata", "prettierrc.toml"),
			},
			outFS: outFilesTabWidth4,
		},
	}

	r := runner.NewRunner()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			if err := fs.WalkDir(testFiles, ".", func(path string, d fs.DirEntry, err error) error {
				if d.IsDir() {
					return nil
				}

				c, _ := fs.ReadFile(testFiles, path)
				if err := os.WriteFile(filepath.Join(dir, path), c, 0o644); err != nil {
					return fmt.Errorf("failed to write to temp dir: %w", err)
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}

			args := tc.args
			args.Patterns = append(args.Patterns, dir)
			if err := r.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}

			if err := fs.WalkDir(tc.outFS, ".", func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}

				if d.IsDir() {
					return nil
				}

				got, err := os.ReadFile(filepath.Join(dir, path))
				if err != nil {
					return fmt.Errorf("failed to read from temp dir: %w", err)
				}

				want, _ := fs.ReadFile(tc.outFS, path)
				if string(got) != string(want) {
					t.Errorf("%s - got: %s, want: %s", path, got, want)
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
