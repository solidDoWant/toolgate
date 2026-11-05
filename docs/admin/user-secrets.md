# User secrets

For: admins who deploy the gateway and keep its encryption keys.

A user secret is a value that a user stores in the gateway, such as a GitHub token. A template uses it as `${secret:NAME}`. The gateway stores each value in PostgreSQL, encrypted. Each user can read, list and delete only their own secrets. The secrets API never returns a value. Only the gateway reads a value, when it starts an instance for that user.

## The keyset

A keyset is a file of encryption keys in the format of [Tink](https://developers.google.com/tink). One key in it is the primary key. The gateway encrypts each new value with the primary key. It decrypts a value with the key that encrypted it.

The keyset is not stored in PostgreSQL. A person who reads the database cannot decrypt the values without the keyset.

To make a keyset:

1. Install `tinkey`, the command line tool of Tink.
2. Run `tinkey create-keyset --key-template AES256_GCM --out-format json --out keyset.json`.
3. Store `keyset.json` in a Kubernetes Secret in the namespace of the gateway.
4. Mount the Secret into the gateway pod.
5. Set `secretsKeysetFile` to the path of the mounted file. See the [configuration reference](configuration.md).

Do not lose the keyset. Without it, nobody can decrypt the stored secrets, and each user must set them again.

## Rotate the key

A new keyset version is the old keyset with a new primary key added. The old keys stay in it, so the old values still decrypt.

1. Run `tinkey add-key --key-template AES256_GCM --in keyset.json --out keyset-new.json`.
2. Run `tinkey list-keyset --in keyset-new.json` to find the ID of the new key.
3. Run `tinkey promote-key --key-id ID --in keyset-new.json --out keyset-rotated.json`.
4. Replace the keyset in the Kubernetes Secret with `keyset-rotated.json`.
5. Restart every gateway pod, so that each one encrypts with the new primary key.
6. Run `toolgate admin rewrap --config FILE` with the configuration of the gateway, in a pod that mounts the new keyset. It encrypts each value again with the primary key and prints the number of values that it changed.
7. Remove the old keys from the keyset with `tinkey delete-key`, then update the Secret and restart the gateway pods again.

If `toolgate admin rewrap` stops with an error, it names the secret that it could not decrypt. The command is safe to run again.
