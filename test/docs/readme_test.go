package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three READMEs (ADR-0010): the newcomer guide in Traditional Chinese
// at the root, its English twin, and the engineer's index under docs/.
var readmes = []string{"README.md", "README.en.md", filepath.Join("docs", "README.md")}

// TestReadmesPointAtFilesThatExist: a README is the first thing a stranger
// reads, so a dead link there is worse than one anywhere else. Links are
// resolved from the file's own directory, backticked repository paths from
// the root, the same way the runbook test does it.
func TestReadmesPointAtFilesThatExist(t *testing.T) {
	for _, name := range readmes {
		f := filepath.Join(repoRoot, name)
		body := read(t, f)
		for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			_, err := os.Stat(filepath.Join(filepath.Dir(f), target))
			assert.NoError(t, err, "%s links to %s", name, target)
		}
		for _, m := range backtickRe.FindAllStringSubmatch(body, -1) {
			if !repoPathRe.MatchString(m[1]) {
				continue
			}
			_, err := os.Stat(filepath.Join(repoRoot, m[1]))
			assert.NoError(t, err, "%s names %s", name, m[1])
		}
	}
}

var (
	fenceBlockRe = regexp.MustCompile("(?s)```[a-z]*\n.*?```")
	imageRe      = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)\)`)
)

// TestReadmeTranslationsHaveTheSameShape: the Chinese and English newcomer
// guides are the same document in two languages. Same number of sections,
// the same code blocks byte for byte (a command never depends on the
// reader's language; explanations go outside the block), the same images,
// and each points at the other and at the engineer's README.
func TestReadmeTranslationsHaveTheSameShape(t *testing.T) {
	zh := read(t, filepath.Join(repoRoot, "README.md"))
	en := read(t, filepath.Join(repoRoot, "README.en.md"))

	assert.Equal(t, len(h2Re.FindAllString(zh, -1)), len(h2Re.FindAllString(en, -1)), "the same number of ## sections")
	assert.Len(t, h1Re.FindAllString(zh, -1), 1)
	assert.Len(t, h1Re.FindAllString(en, -1), 1)

	zhBlocks, enBlocks := fenceBlockRe.FindAllString(zh, -1), fenceBlockRe.FindAllString(en, -1)
	require.Equal(t, len(zhBlocks), len(enBlocks), "the same number of code blocks")
	require.NotEmpty(t, zhBlocks)
	for i := range zhBlocks {
		assert.Equal(t, zhBlocks[i], enBlocks[i], "code block %d differs between the languages", i+1)
	}
	images := func(body string) []string {
		var out []string
		for _, m := range imageRe.FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}
	assert.Equal(t, images(zh), images(en), "the same screenshots")

	assert.Contains(t, zh, "](README.en.md)", "the Chinese guide links to the English one")
	assert.Contains(t, en, "](README.md)", "the English guide links to the Chinese one")
	for _, body := range []string{zh, en} {
		assert.Contains(t, body, "](docs/README.md)", "the newcomer guides point at the engineer's README")
	}
	eng := read(t, filepath.Join(repoRoot, "docs", "README.md"))
	assert.Contains(t, eng, "](../README.md)")
	assert.Contains(t, eng, "](../README.en.md)")

	// the guides make claims about the Makefile; the targets they type exist
	mk := read(t, filepath.Join(repoRoot, "Makefile"))
	for _, target := range []string{"gen-dev-secrets", "up-single", "ps", "faucet", "totp-enroll", "logs", "down", "reset"} {
		assert.Contains(t, zh, "make "+target)
		assert.Regexp(t, "(?m)^"+regexp.QuoteMeta(target)+":", mk, "the README types make %s, which the Makefile lacks", target)
	}
}
