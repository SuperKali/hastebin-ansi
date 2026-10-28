package handler

import "strings"

// languageByExtension names the language of common file extensions, with
// the highlight.js names the web page uses. Logs are ANSI text.
var languageByExtension = map[string]string{
	"ans": "ansi", "ansi": "ansi", "log": "ansi", "bash": "bash", "sh": "bash", "zsh": "bash", "c": "c",
	"h": "c", "cc": "cpp", "cpp": "cpp", "cxx": "cpp", "hpp": "cpp", "cs": "csharp", "css": "css",
	"diff": "diff", "patch": "diff", "dts": "dts", "dtsi": "dts", "erl": "erlang", "go": "go", "hs": "haskell",
	"htm": "html", "html": "html", "cfg": "ini", "conf": "ini", "ini": "ini", "java": "java",
	"js": "javascript", "mjs": "javascript", "json": "json", "kt": "kotlin", "lua": "lua", "mk": "makefile",
	"md": "markdown", "pl": "perl", "php": "php", "py": "python", "rb": "ruby", "rs": "rust", "sql": "sql",
	"swift": "swift", "tex": "tex", "txt": "text", "toml": "toml", "ts": "typescript", "xml": "xml",
	"yaml": "yaml", "yml": "yaml",
}

// extensionByLanguage is the file extension a download of a language gets
var extensionByLanguage = map[string]string{
	"ansi": "log", "bash": "sh", "c": "c", "cpp": "cpp", "csharp": "cs", "css": "css", "diff": "diff",
	"dts": "dts", "erlang": "erl", "go": "go", "haskell": "hs", "html": "html", "ini": "ini", "java": "java",
	"javascript": "js", "json": "json", "kotlin": "kt", "lua": "lua", "makefile": "mk", "markdown": "md",
	"perl": "pl", "php": "php", "python": "py", "ruby": "rb", "rust": "rs", "sql": "sql", "swift": "swift",
	"tex": "tex", "text": "txt", "toml": "toml", "typescript": "ts", "xml": "xml", "yaml": "yml",
}

// LanguageOfExtension returns the language of a file extension such as
// "py" or ".PY": a known one by name, any other of 1 to 10 ASCII letters and
// digits as itself, lowercase; "" when ext is not usable.
func LanguageOfExtension(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	if lang, ok := languageByExtension[ext]; ok {
		return lang
	}
	if safeExtension(ext) {
		return ext
	}
	return ""
}

// ExtensionOfLanguage returns the file extension of a language: a known one,
// a language that is itself a usable extension as is, else ""
func ExtensionOfLanguage(language string) string {
	language = strings.ToLower(language)
	if ext, ok := extensionByLanguage[language]; ok {
		return ext
	}
	if safeExtension(language) {
		return language
	}
	return ""
}
