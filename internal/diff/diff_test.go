package diff

import "testing"

const patch = `@@ -1,4 +1,5 @@
 package main
-import "fmt"
+import (
+	"fmt"
+)
 
 func main() {
@@ -20,2 +21,3 @@ func helper() {
 	a := 1
+	b := 2
 	return
\ No newline at end of file`

func TestParse(t *testing.T) {
	f := Parse("main.go", patch)

	added := f.AddedLines()
	wantAdded := map[int]string{2: "import (", 3: "\t\"fmt\"", 4: ")", 22: "\tb := 2"}
	if len(added) != len(wantAdded) {
		t.Fatalf("added = %+v", added)
	}
	for _, l := range added {
		if wantAdded[l.NewLine] != l.Text {
			t.Errorf("line %d = %q, want %q", l.NewLine, l.Text, wantAdded[l.NewLine])
		}
	}

	for line, want := range map[int]bool{1: true, 2: true, 5: true, 21: true, 22: true, 23: true, 10: false, 24: false} {
		if got := f.Commentable(line); got != want {
			t.Errorf("Commentable(%d) = %v, want %v", line, got, want)
		}
	}
}

func TestAnnotated(t *testing.T) {
	got := Parse("x", "@@ -1,2 +1,2 @@\n-old\n+new\n same").Annotated()
	want := "-        | old\n+      1 | new\n       2 | same\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
