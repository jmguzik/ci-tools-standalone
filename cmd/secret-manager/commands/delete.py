# Ignore dynamic imports
# pylint: disable=E0401, C0413

import click
from google.api_core.exceptions import NotFound, PermissionDenied
from google.cloud import secretmanager
from util import (
    PROJECT_ID,
    get_secret_name,
    is_login_error,
    validate_collection,
    validate_path,
)


@click.command("delete")
@click.option(
    "-c",
    "--collection",
    required=True,
    help="The collection the secret belongs to.",
    type=str,
    callback=validate_collection,
)
@click.argument("secret_path", required=True, callback=validate_path, metavar="SECRET_PATH")
def delete(collection: str, secret_path: str):
    """Delete a secret from the specified collection.

    The SECRET_PATH should be in the format 'group/field' (e.g., 'aws/password').
    """

    client = secretmanager.SecretManagerServiceClient()

    try:
        click.echo(f"Deleting secret '{secret_path}'...")
        client.delete_secret(
            name=client.secret_path(PROJECT_ID, get_secret_name(collection, secret_path))
        )
    except NotFound:
        raise click.ClickException(
            f"Secret '{secret_path}' does not exist within the collection."
        )
    except PermissionDenied:
        raise click.ClickException(
            f"You don't have permission to delete secrets in collection '{collection}'."
        )
    except Exception as e:
        if is_login_error(e):
            raise  # reported by main
        raise click.ClickException(
            f"Failed to delete secret '{secret_path}': {e}. Please retry the delete operation."
        )

    click.echo(f"Secret '{secret_path}' successfully deleted.")
