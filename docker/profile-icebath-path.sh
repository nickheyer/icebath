# Re-adds the image command dirs a login shell's /etc/profile reset drops from PATH
for __icebath_p in /opt/data/.local/bin /command /opt/icebath; do
    case ":$PATH:" in
        *":$__icebath_p:"*) ;;
        *) PATH="$__icebath_p:$PATH" ;;
    esac
done
unset __icebath_p
export PATH
