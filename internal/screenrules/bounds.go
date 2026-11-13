package screenrules

import "regexp/syntax"

// programSize is how many instructions a regexp compiles to once its repeats are unrolled. A pattern too large to unroll is
// estimated, not unrolled, so that measuring a pathological one never costs what running it would: the estimate is the
// size it would have, and past maxProgram either way.
func programSize(source string) (int, error) {
	re, err := syntax.Parse(source, syntax.Perl)
	if err != nil {
		return 0, err
	}

	est := weight(re)
	if est > 4*maxProgram {
		return est, nil
	}

	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return 0, err
	}

	return len(prog.Inst), nil
}

// weight estimates the instructions of a parsed regexp, repeats multiplied out, saturating well above the bound.
func weight(re *syntax.Regexp) int {
	const ceiling = 1 << 20

	n := 1

	switch re.Op {
	case syntax.OpRepeat:
		times := re.Max
		if times < 0 {
			times = re.Min + 1
		}

		inner := 0
		for _, sub := range re.Sub {
			inner += weight(sub)
		}

		return min(ceiling, max(times, 1)*inner+1)
	case syntax.OpLiteral:
		return len(re.Rune)
	}

	for _, sub := range re.Sub {
		n += weight(sub)
		if n >= ceiling {
			return ceiling
		}
	}

	return n
}
