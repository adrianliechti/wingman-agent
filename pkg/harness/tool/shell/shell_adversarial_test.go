package shell

import "testing"

func TestAdversarialReadOnlyRules(t *testing.T) {
	for _, command := range []string{
		`uniq input.txt output.txt`, `xxd input.bin output.hex`,
		`time -o timing.txt cat input.txt`, `command time --output=timing.txt cat input.txt`,
		`command ./env cat input.txt`, `nice env PATH=./bin ls`,
		`command env BASH_ENV=./startup bash -c ':'`,
		`printf '%s\n' '-o output.txt' | xargs sort input.txt`,
		`npm version patch`, `pnpm version patch`, `yarn version --new-version 2.0.0`,
		`poetry version minor`, `pdm config python.use_venv false`,
		`npx which touch marker`, `terraform providers lock`, `tofu providers mirror cache`,
		`kustomize cfg set . replicas 0`, `bundle viz --file graph`,
		`gradle tasks`, `mvn dependency:tree`,
		`fd pattern --exec=touch marker`, `fd pattern -xtouch marker`,
		`git tag --delete --list v1`, `terraform providers schema -json`,
		`f() { echo harmless; }`, `cat ~/.ssh/id_ed25519`,
		`npm audit "$ACTION"`, `git tag --list "$FLAGS"`,
	} {
		t.Run(command, func(t *testing.T) {
			if isReadOnlyCommandDialect(command, dialectBash) {
				t.Fatal("write-capable command classified read-only")
			}
		})
	}
}

func TestAdversarialCommandsRequireReview(t *testing.T) {
	for _, command := range []string{
		`echo payload > .git/config`, `echo payload > sub/.agents/agent.md`,
		`tee .vscode/settings.json`, `touch .envrc`, `mkdir .claude`,
		`cp payload .git/hooks/pre-commit`, `cp -t .git/hooks payload`,
		`mv .agents ordinary`, `rm .envrc`,
		`echo payload > .bashrc`, `tee /etc/profile`, `touch .zshrc`,
		`mkdir /etc/fixture`, `cp payload ~/.config/git/config`,
		`cp -t /etc/fixture payload`, `cp --target-directory=/etc/fixture payload`,
		`mv .bashrc ordinary`, `rm .zshrc`,
		`find . -delete`, `find . -name '*.pyc' -delete`,
		`git restore file.txt`, `git checkout -- file.txt`, `git push origin +HEAD:main`,
		`git -cfoo.bar=baz status`, `rm --rec fixture`,
		`command env BASH_ENV=./startup bash -c ':'`,
		`nice env GIT_SSH_COMMAND=./helper git ls-remote origin`,
		`fd pattern -xrm -rf fixture`, `cat <> .git/config`, `cat <> .bashrc`,
		`cat < ~/.ssh/id_ed25519`,
		`find . -exec rm {} +`, `find . -type f -exec rm -f {} +`, `find . -execdir rm {} \;`,
		`find . -ok /bin/unlink {} \;`, `find . -okdir command rm {} \;`,
		`find src -exec shred -u {} +`, `shred -u secrets.txt`,
		`rsync -a --delete empty/ src/`, `rsync -a --del empty/ src/`, `rsync -a --remove-source-files src/ dst/`,
		`git push --delete origin main`, `git push -d origin release`, `git push origin :main`,
		`git push origin :refs/tags/v1`, `git push --mirror backup`, `git push --prune origin 'refs/heads/*:refs/heads/*'`,
		`git checkout .`, `git checkout HEAD :/`, `git stash drop`, `git stash clear`, `git -C repo stash drop 1`,
	} {
		t.Run(command, func(t *testing.T) {
			if !isDangerousCommandDialect(command, dialectBash) {
				t.Fatal("command bypassed review")
			}
		})
	}
}

func TestAdversarialBenignCounterexamples(t *testing.T) {
	for _, command := range []string{
		`uniq -c input.txt`, `xxd -l 32 input.bin`, `time cat input.txt`,
		`nice -n 5 env LANG=C cat input.txt`, `git status --short`,
		`git branch --list 'feature/*'`, `git tag --list 'v*'`,
		`npm version`, `npm audit --json`, `poetry version --short`,
		`terraform providers`, `git branch -l 'feature/*'`,
		`cat .git/config`, `echo '.agents'`, `printf '%s' 'rm --recursive fixture'`,
	} {
		t.Run(command, func(t *testing.T) {
			if isDangerousCommandDialect(command, dialectBash) || !isReadOnlyCommandDialect(command, dialectBash) {
				t.Fatal("benign inspection should remain read-only without review")
			}
		})
	}
	for _, command := range []string{`echo payload > ordinary.txt`, `cp input.txt output.txt`, `git restore --staged file.txt`, `find . -name '*.pyc' -print`, `cat < .git/config`, `cat < '.agents/a>b'`, `cat < /etc/profile`, `cat < '/etc/a>b'`,
		`git push origin feature/login`, `git push -u origin HEAD`, `git checkout main`, `git checkout -b feature/login`,
		`git stash`, `git stash list`, `git stash pop`, `rsync -a --delay-updates src/ dst/`, `find . -exec grep -l TODO {} +`,
		`find . -name '*.go' -exec gofmt -l {} +`, `find . -empty -exec rmdir {} +`} {
		if isDangerousCommandDialect(command, dialectBash) {
			t.Errorf("routine mutation/inspection requires review: %s", command)
		}
	}
}
