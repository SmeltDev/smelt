package shape

// Classify has a branch for everything, so it is complex.
func Classify(n int, s string) string {
	switch {
	case n < 0:
		return "negative"
	case n == 0:
		return "zero"
	case n < 10 && s == "":
		return "small"
	case n < 100 || s == "x":
		return "medium"
	}
	for i := 0; i < n; i++ {
		if i%2 == 0 && i%3 == 0 {
			return "six"
		}
		if i%5 == 0 {
			return "five"
		}
	}
	for _, c := range s {
		if c == 'a' {
			return "a"
		} else if c == 'b' {
			return "b"
		}
	}
	return "large"
}

// Add is simple.
func Add(a, b int) int {
	return a + b
}
