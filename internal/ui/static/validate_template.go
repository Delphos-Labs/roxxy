package static

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ValidateHTMLTemplate validates HTML template structure
func ValidateHTMLTemplate(templatePath string) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("failed to read template: %w", err)
	}

	content := string(data)
	errors := []string{}

	// Check for balanced template tags
	templateOpen := strings.Count(content, "<template")
	templateClose := strings.Count(content, "</template>")
	if templateOpen != templateClose {
		errors = append(errors, fmt.Sprintf("unbalanced <template> tags: %d open, %d close", templateOpen, templateClose))
	}

	// Check for balanced div tags (basic check)
	divOpen := strings.Count(content, "<div")
	divClose := strings.Count(content, "</div>")
	if divOpen != divClose {
		errors = append(errors, fmt.Sprintf("unbalanced <div> tags: %d open, %d close", divOpen, divClose))
	}

	// Check for Alpine.js x-for/x-if nesting issues
	// Problem: <template x-for> containing <template x-if> can cause issues
	// Solution: Use x-show on div instead of x-if on template
	xForPattern := regexp.MustCompile(`<template[^>]*\s+x-for=`)
	xIfPattern := regexp.MustCompile(`<template[^>]*\s+x-if=`)

	// Find all x-for templates
	xForMatches := xForPattern.FindAllStringIndex(content, -1)

	// Check if x-for template directly contains x-if template (problematic pattern)
	for _, forMatch := range xForMatches {
		// Find the opening tag end
		openTagEnd := strings.Index(content[forMatch[1]:], ">")
		if openTagEnd < 0 {
			continue
		}
		openTagEnd += forMatch[1] + 1

		// Find the matching closing </template> for this x-for
		depth := 1
		pos := openTagEnd
		for pos < len(content) && depth > 0 {
			nextOpen := strings.Index(content[pos:], "<template")
			nextClose := strings.Index(content[pos:], "</template>")

			if nextClose < 0 {
				break
			}
			if nextOpen >= 0 && nextOpen < nextClose {
				depth++
				pos += nextOpen + 8
			} else {
				depth--
				if depth == 0 {
					// Found matching closing tag
					between := content[openTagEnd : pos+nextClose]
					// Check if there's an x-if template directly inside (not nested deeper)
					if xIfPattern.MatchString(between) {
						// Count template depth - if x-if is at depth 1, it's a problem
						ifMatch := xIfPattern.FindStringIndex(between)
						if ifMatch != nil {
							beforeIf := between[:ifMatch[0]]
							templateDepth := strings.Count(beforeIf, "<template") - strings.Count(beforeIf, "</template>")
							if templateDepth == 0 {
								errors = append(errors, fmt.Sprintf("x-for template (line ~%d) contains x-if template - use x-show on div instead", strings.Count(content[:forMatch[0]], "\n")+1))
							}
						}
					}
					break
				}
				pos += nextClose + 10
			}
		}
	}

	// Check for common Alpine.js syntax issues
	// x-show should be on div/span, not template
	xShowOnTemplate := regexp.MustCompile(`<template[^>]*\s+x-show=`)
	if xShowOnTemplate.MatchString(content) {
		errors = append(errors, "x-show found on <template> tag (should be on <div> or other element)")
	}

	// Check for unclosed Alpine.js directives
	directivePattern := regexp.MustCompile(`\s+(x-for|x-if|x-show|x-cloak|@click|:class|:key)=`)
	directives := directivePattern.FindAllString(content, -1)

	// Basic check: ensure quotes are balanced for directives
	for _, dir := range directives {
		// Extract the directive value
		parts := strings.SplitN(dir, "=", 2)
		if len(parts) == 2 {
			value := parts[1]
			// Check if quotes are balanced
			singleQuotes := strings.Count(value, "'")
			doubleQuotes := strings.Count(value, "\"")
			if singleQuotes%2 != 0 && doubleQuotes%2 != 0 {
				errors = append(errors, fmt.Sprintf("unbalanced quotes in directive: %s", dir))
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("template validation failed:\n  %s", strings.Join(errors, "\n  "))
	}

	return nil
}

// ValidateJavaScriptInHTML extracts and validates JavaScript from HTML file
func ValidateJavaScriptInHTML(htmlPath string) error {
	data, err := os.ReadFile(htmlPath)
	if err != nil {
		return fmt.Errorf("failed to read HTML file: %w", err)
	}

	content := string(data)
	errors := []string{}

	// Extract all inline script tags (not external scripts with src=)
	scriptPattern := regexp.MustCompile(`<script([^>]*)>([\s\S]*?)</script>`)
	matches := scriptPattern.FindAllStringSubmatch(content, -1)

	for i, match := range matches {
		if len(match) < 3 {
			continue
		}
		attrs := match[1]
		scriptContent := match[2]

		// Skip external scripts (those with src attribute)
		if strings.Contains(attrs, "src=") {
			continue
		}

		// Skip empty scripts
		if strings.TrimSpace(scriptContent) == "" {
			continue
		}

		// Basic JavaScript syntax checks
		// Check for balanced braces (but allow small differences due to braces in strings/comments)
		openBraces := strings.Count(scriptContent, "{")
		closeBraces := strings.Count(scriptContent, "}")
		diff := openBraces - closeBraces
		if diff != 0 && (diff > 1 || diff < -1) {
			// Only error if difference is more than 1 (to account for braces in strings/comments)
			errors = append(errors, fmt.Sprintf("unbalanced braces in script #%d: %d open, %d close (diff: %d)", i+1, openBraces, closeBraces, diff))
		}

		// Check for balanced parentheses
		openParens := strings.Count(scriptContent, "(")
		closeParens := strings.Count(scriptContent, ")")
		if openParens != closeParens {
			errors = append(errors, fmt.Sprintf("unbalanced parentheses in script #%d: %d open, %d close", i+1, openParens, closeParens))
		}

		// Check for balanced brackets
		openBrackets := strings.Count(scriptContent, "[")
		closeBrackets := strings.Count(scriptContent, "]")
		if openBrackets != closeBrackets {
			errors = append(errors, fmt.Sprintf("unbalanced brackets in script #%d: %d open, %d close", i+1, openBrackets, closeBrackets))
		}

		// Check for common syntax errors
		// Unclosed strings (basic check - look for unmatched quotes)
		// This is a simplified check - proper parsing would be more complex
		// Remove comments first to avoid false positives from quotes in comments
		lines := strings.Split(scriptContent, "\n")
		var codeWithoutComments strings.Builder
		for _, line := range lines {
			// Remove single-line comments (// ...)
			commentIdx := strings.Index(line, "//")
			if commentIdx >= 0 {
				line = line[:commentIdx]
			}
			// Remove multi-line comments (/* ... */) - basic check
			for {
				startIdx := strings.Index(line, "/*")
				if startIdx < 0 {
					break
				}
				endIdx := strings.Index(line[startIdx:], "*/")
				if endIdx < 0 {
					// Unclosed comment - just remove from start
					line = line[:startIdx]
					break
				}
				line = line[:startIdx] + line[startIdx+endIdx+2:]
			}
			codeWithoutComments.WriteString(line)
			codeWithoutComments.WriteString("\n")
		}
		cleanCode := codeWithoutComments.String()

		singleQuotes := strings.Count(cleanCode, "'")
		doubleQuotes := strings.Count(cleanCode, "\"")
		// Check if quotes are balanced (accounting for escaped quotes is complex, so this is approximate)
		// Allow small differences due to quotes in template literals or complex string handling
		if singleQuotes%2 != 0 && singleQuotes > 10 {
			// Only error if there are many quotes (to avoid false positives from edge cases)
			errors = append(errors, fmt.Sprintf("unbalanced single quotes in script #%d: %d quotes (may be in strings/comments)", i+1, singleQuotes))
		}
		if doubleQuotes%2 != 0 && doubleQuotes > 10 {
			errors = append(errors, fmt.Sprintf("unbalanced double quotes in script #%d: %d quotes (may be in strings/comments)", i+1, doubleQuotes))
		}

		// Check for common typos
		if strings.Contains(scriptContent, "fucntion") {
			errors = append(errors, fmt.Sprintf("typo 'fucntion' found in script #%d (should be 'function')", i+1))
		}
		if strings.Contains(scriptContent, "retrun") {
			errors = append(errors, fmt.Sprintf("typo 'retrun' found in script #%d (should be 'return')", i+1))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("JavaScript validation failed:\n  %s", strings.Join(errors, "\n  "))
	}

	return nil
}

// ValidateTemplates validates all HTML templates in the static directory
func ValidateTemplates(staticDir string) error {
	templatePath := filepath.Join(staticDir, "components", "package-list.html")
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return fmt.Errorf("template not found: %s", templatePath)
	}

	return ValidateHTMLTemplate(templatePath)
}
