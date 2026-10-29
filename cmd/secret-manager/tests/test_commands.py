"""Test the commands that write to Google Secret Manager directly."""

import sys
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

from click.testing import CliRunner
from google.api_core.exceptions import DeadlineExceeded, NotFound, PermissionDenied, RetryError, ServiceUnavailable

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from commands.create import create as create_command
from commands.delete import delete as delete_command
from commands.describe import describe as describe_command
from commands.get_service_account import get_service_account as get_sa_command
from commands.update import update as update_command

SECRET_ID = "my-collection__aws__key"


def fake_client():
    """fake_client returns a mocked Secret Manager client that builds resource paths."""
    client = MagicMock()
    client.secret_path.side_effect = lambda project, secret: f"projects/{project}/secrets/{secret}"
    return client


class CommandsTest(unittest.TestCase):
    """Exercise create, update, and delete without Google credentials."""

    def run_command(self, command, module, args, client, **patches):
        """run_command invokes a command with a fake Secret Manager client."""
        with patch(f"commands.{module}.secretmanager.SecretManagerServiceClient", return_value=client):
            for name, value in patches.items():
                stack = patch(f"commands.{module}.{name}", return_value=value)
                stack.start()
                self.addCleanup(stack.stop)
            return CliRunner().invoke(command, args)

    def assert_index_untouched(self, client):
        """assert_index_untouched fails if the command read or wrote an index secret."""
        for call in client.mock_calls:
            self.assertNotIn("____index", str(call))
        client.access_secret_version.assert_not_called()

    def test_create(self):
        for name, get_error, want_exit, want_output, want_created in [
            ("new secret is created", NotFound("missing"), 0, "created successfully", True),
            ("existing secret is rejected", None, 1, "already exists", False),
            ("missing permission is reported", PermissionDenied("denied"), 1, "Insufficient permissions", False),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.get_secret.side_effect = get_error
                result = self.run_command(
                    create_command,
                    "create",
                    ["-c", "my-collection", "aws/key", "-l", "value"],
                    client,
                    check_if_collection_exists=True,
                    prompt_for_labels={},
                    prompt_for_annotations={},
                )
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertIn(want_output, result.output)
                self.assertEqual(client.create_secret.called, want_created)
                self.assertEqual(client.add_secret_version.call_count, int(want_created))
                if want_created:
                    self.assertEqual(client.create_secret.call_args.kwargs["request"]["secret_id"], SECRET_ID)
                self.assert_index_untouched(client)

    def test_update(self):
        for name, get_error, want_exit, want_output in [
            ("existing secret gets a new version", None, 0, "updated successfully"),
            ("missing secret is reported", NotFound("missing"), 1, "does not exist in collection"),
            ("missing permission is reported", PermissionDenied("denied"), 1, "don't have permission"),
            ("other Google error is reported", DeadlineExceeded("slow"), 1, "Failed to access secret 'aws/key'"),
            ("expired login is passed on to main", ServiceUnavailable("expired"), 1, ""),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.get_secret.side_effect = get_error
                result = self.run_command(
                    update_command,
                    "update",
                    ["-c", "my-collection", "aws/key", "-l", "value"],
                    client,
                    destroy_previous_versions=None,
                )
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertIn(want_output, result.output)
                self.assertEqual(client.add_secret_version.called, want_exit == 0)
                self.assertEqual(isinstance(result.exception, ServiceUnavailable), isinstance(get_error, ServiceUnavailable))
                if want_exit == 0:
                    self.assertEqual(
                        client.add_secret_version.call_args.kwargs["parent"],
                        f"projects/openshift-ci-secrets/secrets/{SECRET_ID}",
                    )
                self.assert_index_untouched(client)

    def test_delete(self):
        for name, delete_error, want_exit, want_output in [
            ("existing secret is deleted", None, 0, "successfully deleted"),
            ("missing secret is reported", NotFound("missing"), 1, "does not exist"),
            ("missing permission is reported", PermissionDenied("denied"), 1, "don't have permission to delete"),
            ("expired login is passed on to main", ServiceUnavailable("expired"), 1, ""),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.delete_secret.side_effect = delete_error
                result = self.run_command(delete_command, "delete", ["-c", "my-collection", "aws/key"], client)
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertIn(want_output, result.output)
                self.assertEqual(isinstance(result.exception, ServiceUnavailable), isinstance(delete_error, ServiceUnavailable))
                client.delete_secret.assert_called_once_with(
                    name=f"projects/openshift-ci-secrets/secrets/{SECRET_ID}"
                )
                self.assert_index_untouched(client)

    def test_describe(self):
        for name, get_error, want_exit in [
            ("existing secret is described", None, 0),
            ("expired login is passed on to main", ServiceUnavailable("expired"), 1),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.get_secret.side_effect = get_error
                client.get_secret.return_value.labels = {}
                client.get_secret.return_value.annotations = {}
                result = self.run_command(describe_command, "describe", ["-c", "my-collection", "aws/key"], client)
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertEqual(isinstance(result.exception, ServiceUnavailable), get_error is not None)

    def test_get_service_account(self):
        for name, access_error, want_exit in [
            ("key is printed", None, 0),
            ("expired login is passed on to main", ServiceUnavailable("expired"), 1),
            ("retry timeout after an expired login is passed on", RetryError("timeout", ServiceUnavailable("expired")), 1),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.access_secret_version.side_effect = access_error
                client.access_secret_version.return_value.payload.data = b'{"type": "service_account"}'
                result = self.run_command(get_sa_command, "get_service_account", ["-c", "my-collection"], client)
                self.assertEqual(result.exit_code, want_exit, result.output)
                if access_error is None:
                    self.assertIn('"service_account"', result.output)
                else:
                    self.assertIs(result.exception, access_error)


if __name__ == "__main__":
    unittest.main()
