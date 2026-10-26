package gormx_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Compile both a positive control and negative examples from the README.
// Temporary source files stay outside the repository.
func TestLambdaCompileTimeSafety(t *testing.T) {
	if testing.Short() {
		t.Skip("compiler subprocess checks are omitted in short mode")
	}
	const prelude = `package typecheck
import (
    "context"
    "github.com/heicoder-wuyunbin/gormx"
)
type User struct { Age int }
type Order struct { ID uint }
var age = gormx.Field(func(u *User) *int { return &u.Age })
var ctx = context.Background()
`
	cases := []struct {
		name string
		code string
		want string
	}{
		{"valid", `var q = gormx.NewLambda[User]().Where(age.Eq(18))`, ""},
		{"wrong_value_type", `var q = age.Eq("18")`, "cannot use"},
		{"wrong_model", `func f(dao *gormx.DAO[Order]) { _, _ = dao.List(ctx, gormx.NewLambda[User]().Where(age.Eq(18))) }`, "cannot use"},
		{"wrong_like_type", `var q = gormx.Like(age, "%18%")`, "does not match"},
		{"join_valid", `var q = gormx.Bind(gormx.TableOf[User]("u"), age).Eq(18)`, ""},
		{"join_wrong_value", `var q = gormx.Bind(gormx.TableOf[User]("u"), age).Eq("18")`, "cannot use"},
		{"join_wrong_model", `var q = gormx.Bind(gormx.TableOf[Order]("o"), age)`, "does not match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "typecheck.go")
			if err := os.WriteFile(path, []byte(prelude+tc.code), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			output, err := exec.CommandContext(ctx, "go", "test", path).CombinedOutput()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("positive control did not compile: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("expected compiler rejection %q, got %v\n%s", tc.want, err, output)
			}
		})
	}
}
