package strx

// Coalesce returns the value of the first string that is not empty.
func Coalesce(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
