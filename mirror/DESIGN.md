Design notes
============

Atomicity
---------

To update mirrors atomically, mirror directories are pointed by
symlinks, as symlinks can be replaced atomically with rename(2).

Directory structure
-------------------

go-apt-mirror stores all data under a directory specified in the
configuration file.  Under the directory, data are structured as:

```
(root)
    +- .lock              Lock file to prevent running multiple go-apt-mirror.
    +- MIRROR             Symlink to .MIRROR.DATETIME/MIRROR directory.
    +- .MIRROR.DATETIME
        +- info.json      Checksum information.
        +- MIRROR         Directory for MIRROR.
    +- MIRROR2            Symlink to .MIRROR2.DATETIME/MIRROR2 directory.
    +- .MIRROR2.DATETIME
        +- info.json      Checksum information.
        +- MIRROR2        Directory for MIRROR2.
    ...
```

where MIRROR and MIRROR2 are identifiers for each mirror defined
in the configuration file.  DATETIME is the timestamp when go-apt-mirror
starts mirroring.

Failures
--------

Mirrors are updated independently.  If go-apt-mirror fails to update
a mirror, the symlink of the mirror is left unchanged, and the other
mirrors are still updated.  go-apt-mirror exits with an error if any
mirror failed.

A mirror is published only when all of its files are mirrored, so that
published mirrors are always complete and consistent with signed
`Release` files.  If any pool file such as a deb file fails to
download, the update of the mirror fails, and the previous mirror stays
published.  Indices listed in `Release` but not found in the upstream
server are tolerated, as `Release` usually lists indices in compression
formats that are not served.  However, every `Packages` or `Sources`
index scanned for items must be available in at least one supported
compression format.

To make the mirror consistent, the update also fails if:

- neither `InRelease` nor `Release` with `Release.gpg` is found, unless
  `allow_unsigned` is set for the mirror, or
- `Release` and `InRelease` list different indices, which happens when
  they are downloaded while the upstream server is being updated.

Signatures are not verified, so `Release.gpg` downloaded while the
upstream server is being updated may not match `Release`.

Checksum verification
---------------------

go-apt-mirror validates downloaded item with checksums provided by
APT indices such as `Release` or `Packages.gz`.

Reusing items
-------------

go-apt-mirror reuses previously downloaded items if they are unchanged.
In order to check items quickly, go-apt-mirror keeps checksums in
`info.json` file.
