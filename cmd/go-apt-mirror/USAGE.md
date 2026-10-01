How to configure and run go-apt-mirror
======================================

Synopsis
--------

```
go-apt-mirror [options] [MIRROR MIRROR2...]
```

go-apt-mirror is a console application.  
Run it in your shell, or use `sudo -u USER` to run it as USER.

If `MIRROR` arguments are given, go-apt-mirror updates only the specified
Debian repository mirrors.  With no arguments, it updates all mirrors
defined in the configuration file.

Configuration
-------------

go-apt-mirror reads configurations from a [TOML][] file.  
The default location is `/etc/apt/mirror.toml`.

A sample configuration file is available [here](mirror.toml).

Directory layout
----------------

go-apt-mirror stores mirrors under `dir` in the configuration file as
follows:

```
dir/
├── .lock
├── ubuntu -> .ubuntu.20260101_000000/ubuntu
└── .ubuntu.20260101_000000/
```

Each update downloads a mirror into a new directory named
`.<mirror>.<timestamp>`, then switches the symlink `<mirror>` to it.
Clients should access mirrors through the symlinks.

After updates, go-apt-mirror removes `.<mirror>.<timestamp>` directories
not pointed to by any symlink in `dir`.  Other files and directories in
`dir` are kept, so you may put files such as `index.html` or public keys
there.  However, a directory you create must not be named like
`.<mirror>.<timestamp>`.

The mirror of a mirror removed from the configuration is kept while its
symlink exists.  Remove the symlink to have it removed.

Proxy
-----

go-apt-mirror uses HTTP proxy as specified in [`ProxyFromEnvironment`](https://golang.org/pkg/net/http/#ProxyFromEnvironment).

Options
-------

| Option | Default | Description |
| ------ | ------- | ----------- |
| `-f`   | `/etc/apt/mirror.toml` | Configurations |

As `go-apt-cacher` uses [github.com/cybozu-go/well](https://github.com/cybozu-go/well), flags provided by `well` is also available.


[TOML]: https://github.com/toml-lang/toml
