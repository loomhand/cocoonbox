# shellcheck shell=sh
# login shells, cocoonbox rsh's bash -lc among them, run the tools mise installed through its shims
case ":$PATH:" in
*":$HOME/.local/share/mise/shims:"*) ;;
*) PATH="$HOME/.local/share/mise/shims:$PATH" ;;
esac
