FROM python:3.12-slim

WORKDIR /app

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# requirements.txt is a generated lock: every package pinned and hashed, every
# transitive dependency listed. `--require-hashes` makes pip refuse the install
# outright if that stops being true, so the image cannot drift from the lock.
COPY requirements.txt ./
RUN pip install --no-cache-dir --require-hashes -r requirements.txt

COPY quant_service ./quant_service

EXPOSE 8000
CMD ["uvicorn", "quant_service.main:app", "--host", "0.0.0.0", "--port", "8000"]
