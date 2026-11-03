package types

import "errors"

// ErrBranchMemoryPrivate prevents exploratory branch content from entering the
// global memory types through either automatic or manual extraction.
var ErrBranchMemoryPrivate = errors.New("branch conversation memories are private")
