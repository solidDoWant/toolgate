package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToolSearchMode(t *testing.T) {
	tests := map[string]string{
		"": "", "on": "on", "ON": "on", "true": "on", "1": "on", "off": "off", "false": "off", "0": "off", " off ": "off", "maybe": "",
	}

	for value, want := range tests {
		t.Run("value "+value, func(t *testing.T) {
			assert.Equal(t, want, toolSearchMode(value))
		})
	}
}
