# Style Guide

The consumption of vops should be declarative and stateless where possible.
The code of vops should be simple, concise, human-readable, easily testable, and structured and procedural; no heavy object-orienting, mocks, stupid interfaces.

Test suite should be simple, direct, concise, and **deep**. Not wide, deep.
Spend your time creating tests that cover the whole feature, even if it waits for resources, but test the god damn functionality, not the CRUD.
Obviously write unit tests for validations and deep logic functions; just avoid writing just for the sake of having tests.

## Limitations and benefits for vops

- Linux only.
- Systemd only.
- Do not overstep, each tool should do what it's intended and nothing more.
- Testing suite only for linux, docker, nix, golang scripts, bash and bubblewrap are available.
- Use the fucking terminal, use the fucking tools that are available.
- Filesystem should be BTRFS. Check for it if you need any feature that requires it.
- Even though this concentrate an enourmous amount of features, keep it simple and small individually; it's intended to be a simple, production ready helper/cli tool, not a large-scale-k8s-like-system-for-large-teams.
- SQLite is better then postgres, FS is better then storage engines, stdlib is better then dependencies.
- Reinvent the fucking wheel if the wheels on the market are bad; that's the point of this repo.
- go 1.27+ only. It has the new json/v2 and generic methods, so keep that in mind.
- modularity is key, expansability is optional.
- podman-only for the moment.
