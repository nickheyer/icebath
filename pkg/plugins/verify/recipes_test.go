package verify

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func touch(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, root, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePkg(t *testing.T, root string, data *pyjson.Object) {
	t.Helper()
	text, err := pyjson.Dumps(data)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "package.json", text)
}

func detect(t *testing.T, root string) *Recipe {
	t.Helper()
	r, err := DetectRecipe(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ptrStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func ptrInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

func TestPackageManagerDetection(t *testing.T) {
	t.Run("pnpm_lock_wins", func(t *testing.T) {
		root := t.TempDir()
		touch(t, root, "pnpm-lock.yaml", "yarn.lock")
		pm, err := DetectPackageManager(root)
		eq(t, "manager", pm, "pnpm")
		eq(t, "err", err, nil)
	})
	for _, c := range [][2]string{{"bun.lock", "bun"}, {"bun.lockb", "bun"}, {"yarn.lock", "yarn"},
		{"package-lock.json", "npm"}, {"uv.lock", "uv"}, {"poetry.lock", "poetry"}, {"Pipfile.lock", "pipenv"}} {
		t.Run("single_lockfile_"+c[0], func(t *testing.T) {
			root := t.TempDir()
			touch(t, root, c[0])
			pm, _ := DetectPackageManager(root)
			eq(t, "manager", pm, c[1])
		})
	}
	t.Run("no_lockfile", func(t *testing.T) {
		pm, err := DetectPackageManager(t.TempDir())
		eq(t, "manager", pm, "")
		eq(t, "err", err, nil)
	})
}

func TestNodeDetection(t *testing.T) {
	t.Run("nextjs_with_pnpm", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("dependencies", pyjson.ObjectOf("next", "14.0.0"),
			"scripts", pyjson.ObjectOf("dev", "next dev", "build", "next build", "test", "jest")))
		touch(t, root, "pnpm-lock.yaml")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "nextjs")
		eq(t, "bootstrap", r.Bootstrap, []string{"pnpm install"})
		eq(t, "build", r.Build, []string{"pnpm build"})
		eq(t, "test", r.Test, []string{"pnpm test"})
		eq(t, "start", ptrStr(r.Start), "pnpm dev")
		eq(t, "port", ptrInt(r.Port), 3000)
	})
	t.Run("vite_with_yarn", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("devDependencies", pyjson.ObjectOf("vite", "5.0.0"),
			"scripts", pyjson.ObjectOf("dev", "vite", "build", "vite build")))
		touch(t, root, "yarn.lock")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "vite")
		eq(t, "bootstrap", r.Bootstrap, []string{"yarn install"})
		eq(t, "start", ptrStr(r.Start), "yarn dev")
		eq(t, "port", ptrInt(r.Port), 5173)
	})
	t.Run("bun_runner", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("scripts", pyjson.ObjectOf("start", "node server.js", "build", "tsc")))
		touch(t, root, "bun.lockb")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "node")
		eq(t, "bootstrap", r.Bootstrap, []string{"bun install"})
		eq(t, "start", ptrStr(r.Start), "bun run start")
	})
	t.Run("generic_node_defaults_to_npm", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("scripts", pyjson.ObjectOf("test", "mocha")))
		r := detect(t, root)
		eq(t, "bootstrap", r.Bootstrap, []string{"npm install"})
		eq(t, "test", r.Test, []string{"npm run test"})
		eq(t, "start", ptrStr(r.Start), nil)
		eq(t, "port", ptrInt(r.Port), nil)
		eq(t, "evidence", r.Evidence, []string{"Detected package.json", "Scripts: test"})
	})
	t.Run("port_inferred_from_start_command", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("scripts", pyjson.ObjectOf("dev", "node server.js --port 4111")))
		eq(t, "port", ptrInt(detect(t, root).Port), 4111)
		writePkg(t, root, pyjson.ObjectOf("scripts", pyjson.ObjectOf("dev", "PORT=4222 node server.js")))
		eq(t, "env port", ptrInt(detect(t, root).Port), 4222)
		writePkg(t, root, pyjson.ObjectOf("scripts", pyjson.ObjectOf("dev", "XPORT=4222 node -p ٤٣٢١")))
		eq(t, "unicode digits", ptrInt(detect(t, root).Port), 4321)
	})
	t.Run("non_string_start_script_raises_type_error", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("scripts", pyjson.ObjectOf("dev", int64(5))))
		_, err := DetectRecipe(root)
		if err == nil || err.Error() != "expected string or bytes-like object, got 'int'" {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("cra", func(t *testing.T) {
		root := t.TempDir()
		writePkg(t, root, pyjson.ObjectOf("dependencies", pyjson.ObjectOf("react-scripts", "5.0"),
			"scripts", pyjson.ObjectOf("start", "react-scripts start")))
		r := detect(t, root)
		eq(t, "kind", r.Kind, "cra")
		eq(t, "port", ptrInt(r.Port), 3000)
	})
	t.Run("malformed_package_json_falls_through", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "package.json", "{not json")
		writeFile(t, root, "go.mod", "module x\n")
		eq(t, "kind", detect(t, root).Kind, "go")
	})
}

func TestPythonDetection(t *testing.T) {
	t.Run("django_via_manage_py", func(t *testing.T) {
		root := t.TempDir()
		touch(t, root, "manage.py")
		writeFile(t, root, "requirements.txt", "django\n")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "django")
		eq(t, "test", r.Test, []string{"python manage.py test"})
		eq(t, "port", ptrInt(r.Port), 8000)
		eq(t, "start", ptrStr(r.Start), "python manage.py runserver 0.0.0.0:8000")
	})
	t.Run("fastapi_uvicorn", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "requirements.txt", "fastapi\nuvicorn\n")
		touch(t, root, "main.py")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "fastapi")
		eq(t, "start", ptrStr(r.Start), "uvicorn main:app --host 0.0.0.0 --port 8000")
		eq(t, "port", ptrInt(r.Port), 8000)
	})
	t.Run("flask", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "requirements.txt", "flask\n")
		touch(t, root, "app.py")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "flask")
		eq(t, "port", ptrInt(r.Port), 5000)
	})
	t.Run("generic_python_uv", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "pyproject.toml", "[project]\nname='x'\n")
		touch(t, root, "uv.lock")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "python")
		eq(t, "bootstrap", r.Bootstrap, []string{"uv sync"})
	})
	t.Run("generic_python_pyproject_editable_install", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "pyproject.toml", "[project]\nname='x'\n")
		r := detect(t, root)
		eq(t, "bootstrap", r.Bootstrap, []string{"pip install -e ."})
		eq(t, "test", r.Test, []string{"python -m unittest discover"})
	})
	t.Run("pytest_when_tests_dir", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "requirements.txt", "requests\n")
		if err := os.Mkdir(filepath.Join(root, "tests"), 0o755); err != nil {
			t.Fatal(err)
		}
		eq(t, "test", detect(t, root).Test, []string{"pytest"})
	})
	t.Run("undecodable_pyproject_raises", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "pyproject.toml", "name='\xff'\n")
		if _, err := DetectRecipe(root); err == nil {
			t.Fatal("strict utf-8-sig read of pyproject.toml did not raise")
		}
	})
}

func TestOtherEcosystems(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "go.mod", "module example.com/x\n")
		touch(t, root, "main.go")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "go")
		eq(t, "build", r.Build, []string{"go build ./..."})
		eq(t, "start", ptrStr(r.Start), "go run .")
	})
	t.Run("rust", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "Cargo.toml", "[package]\nname='x'\n")
		touch(t, root, filepath.Join("src", "main.rs"))
		r := detect(t, root)
		eq(t, "kind", r.Kind, "rust")
		eq(t, "start", ptrStr(r.Start), "cargo run")
	})
	t.Run("rust_library_has_no_start", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "Cargo.toml", "[package]\nname='x'\n")
		eq(t, "start", ptrStr(detect(t, root).Start), nil)
	})
	t.Run("maven", func(t *testing.T) {
		root := t.TempDir()
		touch(t, root, "pom.xml")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "maven")
		eq(t, "build", r.Build, []string{"mvn package"})
	})
	t.Run("gradle_wrapper", func(t *testing.T) {
		root := t.TempDir()
		touch(t, root, "build.gradle", "gradlew")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "gradle")
		eq(t, "build", r.Build, []string{"./gradlew build"})
	})
	t.Run("makefile", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "Makefile", "install:\n\tpip install .\nbuild:\n\tmake -C src\ntest:\n\tpytest\nrun:\n\t./app\n")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "make")
		eq(t, "bootstrap", r.Bootstrap, []string{"make install"})
		eq(t, "build", r.Build, []string{"make build"})
		eq(t, "test", r.Test, []string{"make test"})
		eq(t, "start", ptrStr(r.Start), "make run")
		eq(t, "evidence", r.Evidence, []string{"Detected Makefile", "Targets: install, build, test, run"})
	})
	t.Run("docker_compose", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "docker-compose.yml", "services: {}\n")
		r := detect(t, root)
		eq(t, "kind", r.Kind, "compose")
		eq(t, "start", ptrStr(r.Start), "docker compose up")
	})
	t.Run("empty_dir_returns_none", func(t *testing.T) {
		if r := detect(t, t.TempDir()); r != nil {
			t.Fatalf("recipe %#v", r)
		}
	})
}

// Dataclass equality, two recipes equal when their manifest mappings are
func recipeEqual(t *testing.T, got, want *Recipe) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("recipe %v want %v", got, want)
	}
	a, _ := pyjson.Dumps(got.ToDict())
	b, _ := pyjson.Dumps(want.ToDict())
	if a != b {
		t.Fatalf("recipe %s\nwant %s", a, b)
	}
}

func fromDict(t *testing.T, raw any) *Recipe {
	t.Helper()
	r, err := RecipeFromDict(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecipeFromDict(t *testing.T) {
	t.Run("roundtrip", func(t *testing.T) {
		r := NewRecipe("X")
		r.Kind, r.Bootstrap, r.Port, r.Start = "node", []string{"npm install"}, intPtr(3000), strPtr("npm run dev")
		recipeEqual(t, fromDict(t, r.ToDict()), r)
	})
	t.Run("tolerates_garbage", func(t *testing.T) {
		for _, raw := range []any{nil, []any{}, pyjson.ObjectOf("kind", "x"), pyjson.ObjectOf("name", "  ")} {
			if r := fromDict(t, raw); r != nil {
				t.Errorf("%#v gave %#v", raw, r)
			}
		}
	})
	t.Run("grok_style_keys", func(t *testing.T) {
		r := fromDict(t, pyjson.ObjectOf("appLabel", "Next.js", "appKind", "nextjs",
			"installCommands", []any{"npm install"}, "buildCommands", []any{"npm run build"},
			"testCommands", []any{"npm test"}, "startCommand", "npm run dev", "startPort", "3000"))
		eq(t, "name", r.Name, "Next.js")
		eq(t, "port", ptrInt(r.Port), 3000)
		eq(t, "bootstrap", r.Bootstrap, []string{"npm install"})
	})
	t.Run("invalid_port_dropped", func(t *testing.T) {
		eq(t, "port", ptrInt(fromDict(t, pyjson.ObjectOf("name", "x", "port", "not-a-port")).Port), nil)
		eq(t, "port", ptrInt(fromDict(t, pyjson.ObjectOf("name", "x", "port", int64(99999999))).Port), nil)
		eq(t, "float port", ptrInt(fromDict(t, pyjson.ObjectOf("name", "x", "port", 3000.0)).Port), nil)
		r := fromDict(t, pyjson.ObjectOf("name", "x", "port", true))
		text, _ := pyjson.Dumps(r.ToDict().Vals["port"])
		eq(t, "bool port", text+r.portText(), "trueTrue")
	})
	t.Run("bad_readiness_path_normalized", func(t *testing.T) {
		eq(t, "path", fromDict(t, pyjson.ObjectOf("name", "x", "readinessPath", "health")).ReadinessPath, "/")
		eq(t, "path", fromDict(t, pyjson.ObjectOf("name", "x", "readinessPath", "/health")).ReadinessPath, "/health")
	})
}
