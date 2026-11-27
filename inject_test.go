package main

import "testing"

func TestInjectRuntimeFirst(t *testing.T) {
	for in, want := range map[string]string{
		"<!-- <head> --><p>x":                "<script>R</script><!-- <head> --><p>x",
		"<!doctype html><!--<head>--><html>": "<!doctype html><script>R</script><!--<head>--><html>",
		" <!DOCTYPE html>\n<head><title>t":   " <!DOCTYPE html><script>R</script>\n<head><title>t",
		"<!-- x --><!doctype html><head>":    "<script>R</script><!-- x --><!doctype html><head>",
		"<textarea><head></textarea><head>":  "<script>R</script><textarea><head></textarea><head>",
	} {
		if got := injectRuntime(in, "R"); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
}
