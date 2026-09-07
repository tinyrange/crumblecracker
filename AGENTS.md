# Product repository guidance

CrumbleCracker is an independent, in-process runtime for SquadVM and
NeurodeskAppX. Keep only product requirements here. The experimental runtime and
full daemon live in tinyrange/cc; do not add synchronization machinery.

Preserve user storage, settings and lifecycle behavior. Test objective outcomes:
command routing, guest execution, persistent data, input bytes and frame lifetime.
Avoid tests for incidental prose, exact helper construction or convenience defaults.
Keep commit CI focused and fast. Use isolated development caches for VM smoke
checks; never stop unrelated user sessions or clean global VM caches.
