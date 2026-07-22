# vhs tape for docs/assets/demo.gif — a recruiter-facing recording of the
# interactive demo. Regenerate with `make demo-gif` (requires vhs:
# `brew install vhs`) AFTER the demo stack is up (`make demo-up`), because the
# tape drives scenarios against the live stack in real time.
Output docs/assets/demo.gif

Set Shell bash
Set FontSize 14
Set Width 1200
Set Height 650
Set Padding 12
Set TypingSpeed 35ms
Set PlaybackSpeed 1.5

Type "# AegisRoute — an LLM gateway control plane. Let's poke it."
Enter
Sleep 1.5s

Type "bash scripts/demo.sh cache-storm"
Enter
Sleep 16s

Type "bash scripts/demo.sh idempotency-replay"
Enter
Sleep 14s

Type "# now kill the primary backend mid-traffic:"
Enter
Type "bash scripts/demo.sh backend-outage"
Enter
Sleep 55s

Sleep 3s
