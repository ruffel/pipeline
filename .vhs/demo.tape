# Records .vhs/demo.gif for the README: a run that fails, is resumed, and is
# rolled back, using examples/01-quickstart. Regenerate it with `just gif`.

Output .vhs/demo.gif

Require go

Set Shell zsh
Set FontSize 16
Set Width 860
Set Height 500
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

# Start from nothing, with the build cached so each command answers at once.
Hide
Type "cd examples/01-quickstart && rm -rf site.jsonl site.jsonl.control style.css script.js index.html && go build -o /dev/null . && clear"
Enter
Sleep 8s
Show

Type "go run . run -fail index.html"
Sleep 500ms
Enter
Sleep 6s

Hide
Type "clear"
Enter
Show

Type "go run . run"
Sleep 500ms
Enter
Sleep 6s

Hide
Type "clear"
Enter
Show

Type "go run . rollback"
Sleep 500ms
Enter
Sleep 6s

Hide
Type "clear"
Enter
Show

Type "go run . status"
Sleep 500ms
Enter
Sleep 6s

# Leave nothing behind.
Hide
Type "rm -rf site.jsonl site.jsonl.control style.css script.js index.html"
Enter
Sleep 500ms
