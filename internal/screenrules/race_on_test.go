//go:build race

package screenrules

// raceBuild is whether the tests run under the race detector, which slows regexp matching by an order of magnitude: a bound on
// time is not asserted then.
const raceBuild = true
