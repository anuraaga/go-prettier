package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/BurntSushi/toml"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/wasilibs/go-prettier/internal/wasm"
)

var (
	errCheckFailed       = errors.New("check failed")
	errInvalidConfigFile = errors.New("invalid config file")
)

func NewRunner() *Runner {
	ctx := context.Background()

	rtCfg := wazero.NewRuntimeConfig()
	uc, err := os.UserCacheDir()
	if err == nil {
		cache, err := wazero.NewCompilationCacheWithDir(filepath.Join(uc, "com.github.wasilibs"))
		if err == nil {
			rtCfg = rtCfg.WithCompilationCache(cache)
		}
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	compiled, err := rt.CompileModule(ctx, wasm.Prettier)
	if err != nil {
		// Programming bug
		panic(err)
	}

	return &Runner{
		compiled: compiled,
		rt:       rt,
	}
}

type Runner struct {
	compiled wazero.CompiledModule
	rt       wazero.Runtime
}

type RunArgs struct {
	Patterns                  []string
	Config                    string
	NoConfig                  bool
	Check                     bool
	IgnorePaths               []string
	Write                     bool
	WithNodeModules           bool
	NoErrorOnUnmatchedPattern bool
}

func (r *Runner) Run(ctx context.Context, args RunArgs) error {
	var cfgPath string

	pCfg := map[string]any{}

	switch {
	case args.Config != "":
		cfg, err := loadConfigFile(ctx, args.Config)
		if err != nil {
			return err
		}
		cfgPath = args.Config
		pCfg = cfg
	case args.NoConfig:
		// Do nothing
	default:
		for _, name := range []string{".prettierrc", ".prettierrc.json", ".prettierrc.yaml", ".prettierrc.yml", ".prettierrc.toml"} {
			if p := findConfigFile(name); p != "" {
				cfg, err := loadConfigFile(ctx, p)
				if err != nil {
					return err
				}
				cfgPath = p
				pCfg = cfg
				break
			}
		}
	}

	paths := expandPatterns(ctx, args, filepath.Dir(cfgPath))

	if args.Check {
		fmt.Println("Checking formatting...")
	}

	var numCheckFailed atomic.Uint32

	var g errgroup.Group
	for _, p := range paths {
		g.Go(func() error {
			if p.error != "" {
				slog.ErrorContext(ctx, p.error)
				return errors.New(p.error)
			}
			err := r.format(ctx, p, maps.Clone(pCfg), args.Check, args.Write)
			if err == errCheckFailed {
				numCheckFailed.Add(1)
			}
			return err
		})
	}
	err := g.Wait()

	if args.Check {
		if n := numCheckFailed.Load(); n > 0 {
			slog.Warn(fmt.Sprintf("Code style issues found in %d files. Run Prettier to fix.", n))
		} else {
			fmt.Println("All matched files use Prettier code style!")
		}
	}

	return err
}

func (r *Runner) format(ctx context.Context, path expandedPath, pCfg map[string]any, check bool, write bool) error {
	pCfg["filepath"] = path.filePath
	pCfgBytes, err := json.Marshal(pCfg)
	if err != nil {
		// Programming bug
		panic(err)
	}

	var out bytes.Buffer

	fi, err := os.Stat(path.filePath)
	if err != nil {
		slog.WarnContext(ctx, fmt.Sprintf(`Unable to read file "%s"`, path.filePath))
		slog.WarnContext(ctx, err.Error())
		return err
	}

	in, err := os.ReadFile(path.filePath)
	if err != nil {
		slog.WarnContext(ctx, fmt.Sprintf(`Unable to read file "%s"`, path.filePath))
		slog.WarnContext(ctx, err.Error())
		return err
	}

	mCfg := wazero.NewModuleConfig().
		WithStderr(os.Stderr).
		WithSysNanosleep().
		WithSysNanotime().
		WithSysWalltime().
		WithRandSource(rand.Reader).
		WithArgs("prettier", string(pCfgBytes)).
		WithStdin(bytes.NewReader(in)).
		WithStdout(&out)

	_, err = r.rt.InstantiateModule(ctx, r.compiled, mCfg)
	if err != nil {
		if se, ok := err.(*sys.ExitError); ok {
			if se.ExitCode() == 10 {
				if !path.ignoreUnknown {
					slog.WarnContext(ctx, fmt.Sprintf(`No parser could be inferred for file "%s".`, path.filePath))
				}
				return nil
			}
		}
		return fmt.Errorf("runner: failed to run prettier: %w", err)
	}

	if write {
		if err := os.WriteFile(path.filePath, out.Bytes(), fi.Mode()); err != nil {
			return fmt.Errorf("runner: failed to write file: %w", err)
		}
	} else if !check {
		fmt.Print(out.String())
	}

	if check && !bytes.Equal(in, out.Bytes()) {
		slog.Warn(path.filePath)
		return errCheckFailed
	}

	return nil
}

func findConfigFile(name string) string {
	dir, err := filepath.Abs(".")
	if err != nil {
		return ""
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return filepath.Join(dir, name)
		}

		parent := filepath.Dir(dir)
		if parent == dir || parent == "" {
			return ""
		}

		dir = parent
	}
}

func loadConfigFile(ctx context.Context, path string) (map[string]any, error) {
	res := map[string]any{}

	pCfgBytes, err := os.ReadFile(path)
	if err != nil {
		slog.WarnContext(ctx, fmt.Sprintf(`Unable to read config file "%s"`, path))
		slog.WarnContext(ctx, err.Error())
		return res, err
	}

	// YAML is superset of JSON so it should be fine to only use YAML to parse.
	err = yaml.Unmarshal(pCfgBytes, &res)
	if err == nil {
		return res, nil
	}

	if err := toml.Unmarshal(pCfgBytes, &res); err == nil {
		return res, nil
	}

	slog.WarnContext(ctx, fmt.Sprintf(`Invalid config file "%s"`, path))
	// JSON / YAML are more common so use it's error rather than TOML's
	slog.WarnContext(ctx, err.Error())
	return res, errInvalidConfigFile
}
