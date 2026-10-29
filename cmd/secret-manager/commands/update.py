# Ignore dynamic imports
# pylint: disable=E0401, C0413

import click
from google.api_core.exceptions import NotFound, PermissionDenied
from google.cloud import secretmanager
from google.cloud.secretmanager import SecretPayload
from util import (
    PROJECT_ID,
    create_payload,
    destroy_previous_versions,
    get_secret_name,
    is_login_error,
    validate_collection,
    validate_path,
    validate_secret_source,
)


@click.command("update")
@click.option(
    "-c",
    "--collection",
    required=True,
    help="The collection the secret belongs to.",
    type=str,
    callback=validate_collection,
)
@click.argument("secret_path", required=True, callback=validate_path, metavar="SECRET_PATH")
@click.option(
    "-f",
    "--from-file",
    default="",
    help="Path to file with secret data.",
    type=click.Path(file_okay=True, dir_okay=False, readable=True),
)
@click.option(
    "-l", "--from-literal", default="", help="Secret data as string input.", type=str
)
def update(collection: str, secret_path: str, from_file: str, from_literal: str):
    """Update an existing secret.

    The SECRET_PATH should be in the format 'group/field' (e.g., 'aws/password').
    """

    validate_secret_source(from_file, from_literal)
    client = secretmanager.SecretManagerServiceClient()

    full_secret_path = client.secret_path(PROJECT_ID, get_secret_name(collection, secret_path))
    try:
        client.get_secret(request={"name": full_secret_path})
    except PermissionDenied as e:
        raise click.ClickException(
            f"You don't have permission to access secrets in collection '{collection}'"
        ) from e
    except NotFound as e:
        raise click.ClickException(
            f"Secret '{secret_path}' does not exist in collection '{collection}'."
        ) from e
    except Exception as e:
        if is_login_error(e):
            raise  # reported by main
        raise click.ClickException(
            f"Failed to access secret '{secret_path}' in collection '{collection}': {e}."
        ) from e

    try:
        new_version = client.add_secret_version(
            parent=full_secret_path,
            payload=SecretPayload(data=create_payload(from_file, from_literal)),
        )
    except PermissionDenied:
        raise click.ClickException(
            f"You don't have permission to update secrets in collection '{collection}'"
        )
    except Exception as e:
        raise click.ClickException(
            f"Failed to update secret '{secret_path}': {e}."
        )

    click.echo(f"Secret '{secret_path}' updated successfully.")

    try:
        destroy_previous_versions(client, full_secret_path, new_version.name)
    except Exception as e:
        click.echo(
            f"Warning: secret '{secret_path}' was updated, but failed to clean up previous versions: {e}",
            err=True,
        )
