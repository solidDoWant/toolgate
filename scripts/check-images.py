#!/usr/bin/env python3
"""Fails on a container image reference that is unqualified or from Docker Hub.

Every image comes from ghcr.io, quay.io or mirror.gcr.io (or a local registry
such as localhost). The check reads Go image constants and Image fields, YAML
image: fields, Dockerfile FROM lines and docker/kind commands in shell
scripts. It does not read SQL, so SQL FROM is never matched.
"""

import os
import pathlib
import re
import subprocess
import sys

DOCKER_HUB = ("docker.io", "index.docker.io", "registry-1.docker.io")
GO_IMAGE = re.compile(r'\b\w*Image\s*(?::=|=|:)\s*"([^"]*)"')
YAML_IMAGE = re.compile(r"^\s*(?:-\s*)?image:\s*[\"']?([^\"'\s#]+)")
DOCKER_FROM = re.compile(r"^\s*FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?", re.IGNORECASE)
SHELL_IMAGE_FLAG = re.compile(r"--image[= ]\s*[\"']?([^\"'\s]+)")
SHELL_DOCKER = re.compile(r"\bdocker\s+(?:run|pull|create)\b(.*)")


def problem(image):
    """Returns why image is not allowed, or None when it is."""
    if "$" in image or "{{" in image or image == "scratch":
        return None  # Filled in at run time, or no base image.
    first, _, rest = image.partition("/")
    if first in DOCKER_HUB:
        return "comes from Docker Hub"
    if not rest or not ("." in first or ":" in first or first == "localhost"):
        return "has no registry host"
    return None


def docker_run_image(arguments):
    """Returns the image of the arguments after docker run, pull or create."""
    words = arguments.split()
    skip = False
    for word in words:
        if skip:
            skip = False
            continue
        if word.startswith("-"):
            # Flags with a separate value; others take none or use =.
            skip = word in ("--name", "-e", "--env", "-v", "--volume", "-p", "--publish", "--entrypoint", "-w", "--network", "--user", "-u")
            continue
        return word.strip("\"'")
    return None


def check_text(path, text):
    """Returns the list of problems found in the text of the file at path."""
    name = pathlib.PurePosixPath(path).name
    found = []
    stages = set()
    for number, line in enumerate(text.splitlines(), 1):
        images = []
        if name.endswith(".go"):
            images = GO_IMAGE.findall(line)
        elif name.endswith((".yaml", ".yml")):
            match = YAML_IMAGE.match(line)
            images = [match.group(1)] if match else []
        elif name == "Dockerfile" or name.startswith("Dockerfile.") or name.endswith(".Dockerfile"):
            match = DOCKER_FROM.match(line)
            if match:
                if match.group(1) not in stages:
                    images = [match.group(1)]
                if match.group(2):
                    stages.add(match.group(2))
        elif name.endswith(".sh"):
            images = SHELL_IMAGE_FLAG.findall(line)
            match = SHELL_DOCKER.search(line)
            if match and (image := docker_run_image(match.group(1))):
                images.append(image)
        for image in images:
            if image == "":
                continue
            reason = problem(image)
            if reason:
                found.append(f"{path}:{number}: image {image!r} {reason}; use ghcr.io, quay.io or mirror.gcr.io, pinned by digest")
    return found


def main():
    root = subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip()
    os.chdir(root)
    files = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard"], text=True
    ).splitlines()
    found = []
    for path in files:
        if path.startswith(".claude/") or not os.path.isfile(path):
            continue
        try:
            text = pathlib.Path(path).read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        found += check_text(path, text)
    for message in found:
        print(message, file=sys.stderr)
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
