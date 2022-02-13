package eql

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestEqlSimple(t *testing.T) {
	input := "fix=path feat=minor bang=major"
	output := map[string]string{
		"fix":  "path",
		"feat": "minor",
		"bang": "major",
	}
	r, err := Parse(input)
	require.NoError(t, err)
	assert.Equal(t, output, r)
}

func TestEqlInvalid(t *testing.T) {
	r, err := Parse(" =path test = true")
	assert.Error(t, err)
	assert.Nil(t, r)
}

func TestEqlSpaced(t *testing.T) {
	input := "fix = path feat = minor bang = major"
	output := map[string]string{
		"fix":  "path",
		"feat": "minor",
		"bang": "major",
	}
	r, err := Parse(input)
	require.NoError(t, err)
	assert.Equal(t, output, r)
}

func TestQuoted(t *testing.T) {
	input := "fix = \"path\" feat = \"minor\" bang = \"major\""
	output := map[string]string{
		"fix":  "path",
		"feat": "minor",
		"bang": "major",
	}
	r, err := Parse(input)
	require.NoError(t, err)
	assert.Equal(t, output, r)
}

func TestQuotedEscape(t *testing.T) {
	input := `test="foo bar" other=test abc="def \" ghi"`
	output := map[string]string{
		"test":  "foo bar",
		"other": "test",
		"abc":   "def \" ghi",
	}
	r, err := Parse(input)
	require.NoError(t, err)
	assert.Equal(t, output, r)
}
