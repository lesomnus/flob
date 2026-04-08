package version

type buildInfo struct {
	Version  string
	GitRev   string
	GitDirty bool
}

//go:generate bash -c "../../../scripts/gen-version-file.sh > /dev/null"
var _buildInfo = buildInfo{
	Version:  "v0.0.0-local",
	GitRev:   "0000000000000000000000000000000000000000",
	GitDirty: false,
}

func Get() buildInfo {
	return _buildInfo
}
