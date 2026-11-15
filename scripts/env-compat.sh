# Sourced by Bash tooling. Explicit VESTRAIL_ values (including empty) win.
for legacy_name in ${!ARTEX_@}; do
  current_name="VESTRAIL_${legacy_name#ARTEX_}"
  if [[ ! -v "$current_name" ]]; then
    printf -v "$current_name" '%s' "${!legacy_name}"
    export "$current_name"
  fi
done
unset legacy_name current_name
