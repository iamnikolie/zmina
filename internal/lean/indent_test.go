package lean

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIndentSure(t *testing.T) {
	cs := "public class Report\n{\n    const string Q = @\"select *\n    from \"\"orders\"\"\n\";\n\n    public void Run()\n    {\n        Go(Q);\n    }\n}\n"
	assert.Equal(t, []string{"0 1-11 Report", "1 7-10 Report.Run"}, names(Outline("r.cs", []byte(cs))), "a verbatim string over lines is no code")
	assert.True(t, csSure([]byte(cs)))
	assert.False(t, csSure([]byte("class A\n{\n    string q = @\"open\n}\n")), "a verbatim string left open")

	java := "class Mail {\n    String body = \"\"\"\n        void fake() {\n        }\n        \"\"\";\n\n    void send() {\n        go();\n    }\n}\n"
	assert.Equal(t, []string{"0 1-10 Mail", "1 7-9 Mail.send"}, names(Outline("Mail.java", []byte(java))), "a text block is no code")
	assert.True(t, csSure([]byte(java)))

	php := "<?php\nclass Mail\n{\n    public $body = 'Dear user,\n    function fake() {\n    }\n';\n\n    public function send()\n    {\n        return 1; /* done\n        */\n    }\n}\n"
	assert.Equal(t, []string{"0 2-14 Mail", "1 9-13 Mail.send"}, names(Outline("m.php", []byte(php))), "a PHP string over lines is no code")
	assert.True(t, phpSure([]byte(php)))
	assert.False(t, phpSure([]byte("<?php\n$a = \"open\n")), "a string left open")

	doc := "<?php\nclass Box\n{\n    /**\n     * @return array{name: the box's,\n     */\n    public function get()\n    {\n        return [];\n    }\n}\n"
	assert.Equal(t, []string{"0 2-11 Box", "1 4-10 Box.get"}, names(Outline("b.php", []byte(doc))), "a quote in a docblock opens nothing")
	assert.True(t, phpSure([]byte(doc)))

	shift := "import os\n\nmask = 1 << ENABLED\n\ndef first():\n    return call(\n        ENABLED,\n    )\n\ndef second():\n    return 2\n"
	assert.Equal(t, []string{"0 5-8 first", "0 10-11 second"}, names(Outline("a.py", []byte(shift))), "<< outside shell, PHP, Ruby, Perl is a shift")

	assert.True(t, vbSure([]byte("Sub A()\n    x = \"say \"\"hi\"\"\" ' it's \"\nEnd Sub\n")), "\"\" inside a string, a quote in a comment")
	assert.False(t, vbSure([]byte("Sub A()\n    x = \"open\nEnd Sub\n")))

	assert.Equal(t, []string{"0 1-4 Box", "1 2-2 Box.Marker", "1 3-3 Box.Color"},
		names(Outline("b.cs", []byte("class Box {\n    public interface Marker { }\n    enum Color { Red, Green }\n}\n"))), "one-line types are listed")
}

func TestGuardReadIndentTypes(t *testing.T) {
	dir := t.TempDir()
	filler := strings.Repeat("// filler line for size\n", 800)
	sure := write(t, dir, "Mail.java", "class Mail {\n    void send() {\n        go();\n    }\n}\n"+filler)
	open := write(t, dir, "Odd.cs", "class Odd\n{\n    string q = @\"open\n}\n"+filler)
	other := write(t, dir, "mail.kt", "class Mail {\n    fun send() {\n        go()\n    }\n}\n"+filler)

	read := func(p string) Decision {
		return Guard(HookInput{Cwd: dir, ToolName: "Read", ToolInput: map[string]any{"file_path": p}}, "/opt/zmina", nil)
	}

	assert.Equal(t, "outline", read(sure).Kind)
	assert.Contains(t, read(sure).Reason, "2-4\tvoid send() {")
	assert.Equal(t, Decision{}, read(open), "a verbatim string left open: no answer")
	assert.Equal(t, Decision{}, read(other), "a type the guard does not read: Read as asked")
}
