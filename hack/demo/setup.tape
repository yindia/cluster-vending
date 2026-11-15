# The hidden setup every phase tape sources (Source hack/demo/setup.tape):
# the prompt, the helpers (lib.sh), and the run's state (demo_init: the
# run DEMO_RUN names, which hack/demo/run.sh sets). Ends shown, with a clear
# screen.

Hide
Type `export PS1='\[\e[1;36m\]❯ \[\e[0m\]' PATH="$PWD/bin:$PATH"` Enter
Type `source hack/demo/lib.sh && demo_init && clear` Enter
Wait@30s /^❯\s*$/
Show
