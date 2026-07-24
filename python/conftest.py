# Placing conftest.py at the project root makes pytest insert this directory
# into sys.path, so `quant_service` imports resolve under a bare `pytest` run
# (the CI invocation) without installing the package.
