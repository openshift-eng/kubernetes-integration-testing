package goja

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModules(t *testing.T) {
	t.Run("require relative module", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js":     `var util = require("./lib/util"); exports.main = function(ctx) { return {exitCode: util.code()}; };`,
			"pkg/lib/util.js": `exports.code = function() { return 0; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("require cross-package", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js":       `var helpers = require("common/helpers"); exports.main = function(ctx) { return {exitCode: helpers.val()}; };`,
			"common/helpers.js": `exports.val = function() { return 0; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("require fs rejected", func(t *testing.T) {
		_, err := runScript(t, map[string]string{
			"pkg/main.js": `var fs = require("fs"); exports.main = function(ctx) {};`,
		}, "pkg/main.js")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not available")
	})

	t.Run("require nonexistent", func(t *testing.T) {
		_, err := runScript(t, map[string]string{
			"pkg/main.js": `var x = require("./nope"); exports.main = function(ctx) {};`,
		}, "pkg/main.js")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot find module")
	})

	t.Run("require index.js", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js":      `var lib = require("./lib"); exports.main = function(ctx) { return {exitCode: lib.val()}; };`,
			"pkg/lib/index.js": `exports.val = function() { return 0; };`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("module cached", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js":    `var a = require("./counter"); var b = require("./counter"); exports.main = function(ctx) { return {exitCode: a.count === b.count ? 0 : 1}; };`,
			"pkg/counter.js": `exports.count = Math.random();`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		_, err := runScript(t, map[string]string{
			"pkg/main.js": `var x = require("../../../etc/passwd"); exports.main = function(ctx) {};`,
		}, "pkg/main.js")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside bundle")
	})

	t.Run("cycle returns partial exports", func(t *testing.T) {
		result, err := runScript(t, map[string]string{
			"pkg/main.js": `var a = require("./a"); exports.main = function(ctx) { return {exitCode: a.val}; };`,
			"pkg/a.js":    `var b = require("./b"); exports.val = 0;`,
			"pkg/b.js":    `var a = require("./a"); exports.aKeys = Object.keys(a).length;`,
		}, "pkg/main.js")
		require.NoError(t, err)
		assert.Equal(t, 0, result.ExitCode)
	})

}
