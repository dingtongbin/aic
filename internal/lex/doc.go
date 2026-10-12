// Package lex implements the AIC lexer.
//
// 29 keywords plus reserved words (true/false/nil — nil only
// legal in Err positions); identifiers start with an ASCII letter or `_` and may
// contain digits and a single underscore, consecutive underscores rejected
// (reserved for mangling); comments //, /* */ (non-nesting) and ///;
// numeric literals with 0x/0b prefixes and _ separators.
//
// H7: the keyword/annotation tables are the frozen syntax surface —
// never extended without the "three irreplaceable use-cases" process.
package lex
