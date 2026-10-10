//go:build !linux

package flob

// nlinkKnown is whether [nlink] reads the count: off Linux it answers 1, which
// is right for an Erase -- removing the shared link loses no content -- and
// wrong for telling an orphan apart from a shared blob.
const nlinkKnown = false

func nlink(p string) (int, error) {
	// Return always 1 results delete of global blob regardless of the number of hard links to the file.
	// Even though the blobs for each repo are still available.
	return 1, nil
}
