#!/bin/sh
# Bundle musl runtime tools so old toolchains need no package-manager edits.
set -eu
kit=/opt/smelt
runtime=$kit/libexec/runtime
mkdir -p "$runtime/bin" "$runtime/lib" "$runtime/git-core" "$runtime/git-core-real"
cp -L /lib/*.so* /usr/lib/*.so* "$runtime/lib/"
cp /etc/ssl/certs/ca-certificates.crt "$runtime/ca-certificates.crt"
cp -R /usr/share/git-core "$runtime/share"
# Sync creates the mounted repository as the host UID, which can differ from
# the sandbox user (including root). Keep trust inside the kit, independent
# of the base image's /etc/gitconfig and without needing to modify it.
printf '[safe]\n\tdirectory = *\n' > "$runtime/gitconfig"
wrap() {
  target=$1
  binary=$2
  cat > "$target" <<WRAPPER
#!/bin/sh
export GIT_CONFIG_SYSTEM=$runtime/gitconfig
export GIT_EXEC_PATH=$runtime/git-core
export GIT_TEMPLATE_DIR=$runtime/share/templates
export GIT_SSL_CAINFO=\${GIT_SSL_CAINFO:-$runtime/ca-certificates.crt}
exec $kit/lib/$(basename /lib/ld-musl-*.so.1) --library-path $runtime/lib $binary "\$@"
WRAPPER
  chmod +x "$target"
}
for tool in git bash pkill sha256sum; do
  cp -L "$(command -v "$tool")" "$runtime/bin/$tool"
  wrap "$kit/bin/$tool" "$runtime/bin/$tool"
done
# Git launches helpers (notably remote-https) as separate executables. Each
# native helper needs the bundled loader too; shell helpers use the base shell.
for helper in "$(git --exec-path)"/*; do
  name=$(basename "$helper")
  if [ -d "$helper" ]; then
    # Git also ships shell resources such as mergetools/ here.
    cp -RL "$helper" "$runtime/git-core/$name"
  elif [ "$(head -c 4 "$helper" | od -An -tx1 | tr -d ' \n')" = 7f454c46 ]; then
    cp -L "$helper" "$runtime/git-core-real/$name"
    wrap "$runtime/git-core/$name" "$runtime/git-core-real/$name"
  else
    cp -L "$helper" "$runtime/git-core/$name"
  fi
done
for tool in git bash pkill sha256sum; do "$kit/bin/$tool" --version; done
