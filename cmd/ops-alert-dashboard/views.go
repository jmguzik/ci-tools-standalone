package main

import (
	"fmt"
	"regexp"
	"strings"
)

// validateChannelName keeps a deployment-owned name readable without showing channel IDs.
func validateChannelName(value string) error {
	name := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`).MatchString(name) {
		return fmt.Errorf("--slack-channel-name must be a Slack channel name")
	}
	return nil
}

// splitAuthors preserves explicit author selection and allows disabling the PR view.
func splitAuthors(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
