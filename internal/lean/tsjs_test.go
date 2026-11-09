package lean

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const tsSrc = `import { Shop } from './shop';
import type {
  Order,
  Line,
} from './order';

/** Prices in cents. */
export type Price =
  | { kind: 'fixed'; cents: number }
  | { kind: 'free' };

export interface Cart {
  lines: Line[];
}

enum Color { Red, Green }

const SLASH = /[{(]/g, GT = />\)\}/u;
const greeting = ` + "`hi ${'{'} ${user.name}`" + `;

@Injectable({
  scope: 'request',
})
export class Till extends Base<
  Order,
  Line
> {
  private readonly total = 0;
  static {
    init();
  }

  constructor(private shop: Shop) {
    super();
  }

  async open(day: string): Promise<void> {
    if (day) {
      return;
    }
  }

  get name(): string {
    return 'till {';
  }

  onClick = (e: Event) => {
    log(e);
  };
}

export const Badge = ({ label }: Props) => {
  return (
    <div className="badge">
      <p>Don't {label}</p>
      <Icon size={2} />
    </div>
  );
};

export function sum(
  a: number,
  b: number,
): number {
  return a + b;
}

namespace Tax {
  export function rate(): number {
    return 0.2;
  }
}

export default {
  name: 'till',
  start() {
    return 1;
  },
};
`

func TestJSOutline(t *testing.T) {
	assert.Equal(t, []string{
		"0 1-5 imports", "0 7-10 Price", "0 12-14 Cart", "0 16-16 Color", "0 18-18 SLASH", "0 19-19 greeting",
		"0 21-50 Till", "1 33-35 Till.constructor", "1 37-41 Till.open", "1 43-45 Till.name", "1 47-49 Till.onClick",
		"0 52-59 Badge", "0 61-66 sum", "0 68-72 Tax", "1 69-71 Tax.rate", "0 74-79 default",
	}, names(Outline("till.tsx", []byte(tsSrc))), "a regexp, a template and JSX text with a quote leave the braces whole")
	assert.True(t, jsSure([]byte(tsSrc)))

	js := "'use strict';\nvar path = require('path');\nvar http = require('http');\n\nvar res = module.exports = {};\n\n" +
		"res.status = function status(code) {\n  this.statusCode = code;\n  return this;\n};\n\nres.contentType =\n" +
		"res.type = function contentType(type) {\n  return this.set(type);\n};\n\ndescribe('res', function () {\n" +
		"  it('sets the status', function () {\n    res.status(201);\n  });\n\n  describe('.type', () => {\n" +
		"    it.skip('sets it', () => {});\n  });\n});\n"
	assert.Equal(t, []string{
		"0 2-3 imports", "0 5-5 res", "0 7-10 res.status", "0 12-15 res.contentType", "0 17-25 res",
		"1 18-20 res.sets the status", "1 22-24 res..type", "2 23-23 res..type.sets it",
	}, names(Outline("res.js", []byte(js))), "CommonJS: require is an import, a function assigned to a member is listed; tests by their titles")

	assert.False(t, jsSure([]byte("function f() {\n  return (1;\n}\n")), "a bracket closed by another kind")
	assert.False(t, jsSure([]byte("function f() {\n  if (x) {\n\nexport function g() {\n}\n")), "a reset at column 0")

	big := "export class Big {\n" + strings.Repeat("  m(): void {\n    x();\n  }\n", 30) + "}\n"
	assert.Len(t, Outline("big.ts", []byte(big)), 31, "a class's members are listed whatever its size")
}

func TestGuardReadJS(t *testing.T) {
	dir := t.TempDir()
	filler := strings.Repeat("// filler line for size\n", 800)
	sure := write(t, dir, "till.tsx", tsSrc+filler)
	unsure := write(t, dir, "odd.js", "function f() {\n  return (1;\n}\n"+filler)

	read := func(p string) Decision {
		return Guard(HookInput{Cwd: dir, ToolName: "Read", ToolInput: map[string]any{"file_path": p}}, "/opt/zmina", nil)
	}

	d := read(sure)
	assert.True(t, jsSure([]byte(tsSrc)))
	assert.Equal(t, "outline", d.Kind)
	assert.Contains(t, d.Reason, "37-41\tasync open(day: string): Promise<void>")
	assert.Equal(t, Decision{}, read(unsure), "brackets that did not close by kind: no answer")
}
