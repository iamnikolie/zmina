package lean

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const rustSrc = `//! The till.

use std::fmt;
use std::collections::{
    HashMap,
    HashSet,
};

/// What a line costs.
#[derive(Debug, Clone)]
pub struct Line<'a> {
    name: &'a str,
    cents: u64,
}

pub enum Brace { Open = b'{' as isize, Close }

const HELP: &str = r#"usage: till {open|close} "now""#;

impl<'a> fmt::Display for Line<'a>
where
    'a: 'static,
{
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let c = '}';
        write!(f, "{}{}", self.name, c)
    }
}

impl Line<'_> {
    pub(crate) const fn cents(&self) -> u64 {
        self.cents
    }
}

pub fn total<I>(lines: I) -> u64
where
    I: IntoIterator<Item = u64>,
{
    lines.into_iter().sum()
}

pub trait Priced {
    fn price(&self) -> u64;
}

macro_rules! line {
    ($n:expr) => { Line { name: $n, cents: 0 } };
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sums() {
        assert_eq!(total(vec![1, 2]), 3);
    }
}

tilltest!(opens_at_nine, |till: Till| {
    till.open();
});
`

func TestRustOutline(t *testing.T) {
	assert.Equal(t, []string{
		"0 3-7 imports", "0 9-14 Line", "0 16-16 Brace", "0 18-18 HELP", "0 20-28 Line", "1 24-27 Line.fmt",
		"0 30-34 Line", "1 31-33 Line.cents", "0 36-41 total", "0 43-45 Priced", "1 44-44 Priced.price",
		"0 47-49 line", "0 51-59 tests", "1 53-53 imports", "1 55-58 tests.sums", "0 61-63 opens_at_nine",
	}, names(Outline("till.rs", []byte(rustSrc))), "char literals, lifetimes and raw strings leave the braces whole; where goes with its fn")
	assert.True(t, rustSure([]byte(rustSrc)))
	assert.False(t, rustSure([]byte("fn f() {\n    let s = \"open\n")), "a string left open")

	assert.Equal(t, "Line", rustImplName("<'a, T: Clone> fmt::Display for &'a mut crate::till::Line<T> where T: Copy {"))
	assert.Equal(t, "Till", rustImplName(" Till {"))

	big := "impl Big {\n" + strings.Repeat("    fn m(&self) {\n        x();\n    }\n", 30) + "}\n"
	assert.Len(t, Outline("big.rs", []byte(big)), 31, "an impl's fns are listed whatever its size")
}
