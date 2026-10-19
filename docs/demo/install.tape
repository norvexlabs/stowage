# The installer of examples/minimal, first install in loopback mode. docs/demo/record.sh replaces
# @REG@ with the throwaway registry, @PULL_CONFIG@ with the Docker config holding its pull login (for
# the installer image's pull alone: the installer pulls with the account baked into it), and runs
# this tape in the operator's home directory.
Output out/install.gif
Set Shell bash
Set FontSize 15
Set Width 1400
Set Height 760
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

Hide
Type "export PS1='\[\e[1;32m\]$\[\e[0m\] '; DOCKER_CONFIG=@PULL_CONFIG@ docker pull -q @REG@/notes/notes-installer >/dev/null; clear" Enter
Sleep 2s
Show

Type "docker run --rm @REG@/notes/notes-installer > notes.sh" Sleep 300ms Enter
Wait+Line /\$$/
Sleep 500ms
Type "sh notes.sh" Sleep 300ms Enter
Wait+Screen@20s /Install into/
Sleep 1.5s
Enter
Wait+Screen@20s /How is it served/
Sleep 1s
Up Sleep 600ms Up Sleep 600ms Up Sleep 600ms Down Sleep 600ms Down Sleep 600ms Down Sleep 1s
Enter
Wait+Screen@20s /Administrator email/
Sleep 1s
Type "admin@notes.example" Sleep 1s
Enter
Wait+Screen@20s /Review/
Sleep 3s
Enter
Wait+Line@180s /\$$/
Sleep 3s
Type "./notes/notesctl status | head -4" Sleep 500ms Enter
Wait+Line@30s /\$$/
Sleep 5s
