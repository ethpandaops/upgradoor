# Throwaway Debian 13 "host" for the upgradoor system tests: systemd as
# PID 1, a local apt repository with dummy packages, no network needed at
# run time. See lib.sh for how it is run and rebooted.
FROM debian:13

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus apt-utils \
 && rm -rf /var/lib/apt/lists/* \
 # The only package sources of this host are the local repositories.
 && rm -f /etc/apt/sources.list /etc/apt/sources.list.d/* \
 # Nothing may run apt behind the test's back or wait for hardware.
 && systemctl mask \
      apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service \
      dpkg-db-backup.timer e2scrub_all.timer fstrim.timer \
      getty.target console-getty.service \
      systemd-firstboot.service systemd-remount-fs.service

COPY build-repo.sh /usr/local/sbin/upgradoor-build-repo
COPY boot.sh /usr/local/sbin/upgradoor-boot
RUN chmod 0755 /usr/local/sbin/upgradoor-build-repo /usr/local/sbin/upgradoor-boot

# copy: (not file:) makes apt keep its own copy of the index in
# /var/lib/apt/lists, as it does for a remote repository, so the lists can
# be stale.
RUN upgradoor-build-repo stage1 \
 && printf '%s\n' \
      'deb [trusted=yes] copy:/opt/upgradoor-repo/main ./' \
      'deb [trusted=yes] copy:/opt/upgradoor-repo/security ./' \
      > /etc/apt/sources.list.d/upgradoor-test.list \
 && apt-get update \
 && apt-get install -y \
      upgradoor-dummy=1.0 upgradoor-late=1.0 upgradoor-broken=1.0 \
      upgradoor-plain=1.0 upgradoor-secfix=1.0 \
 # A person edited the dummy's configuration file.
 && echo 'setting=changed-by-hand' >> /etc/upgradoor-test/upgradoor-dummy.conf \
 # Published after the last "apt-get update": apt does not know it yet.
 && upgradoor-build-repo stage2

ENV container=docker
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/sbin/upgradoor-boot"]
