// Package parse implements the handwritten single-pass LL parser.
//
// 核心设计 §三: no backtracking. The parser accepts the complete grammar of
// all 29 keywords from the very first commit (frozen syntax surface).
//
// Error recovery: on failure, skip to the next statement boundary and
// keep reporting — one compile surfaces all errors, never just the first.
// Hard check at the grammar level: Err only occupies the trailing return
// slot.
package parse
