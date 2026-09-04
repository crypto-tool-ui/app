FROM python:3.11-slim

WORKDIR /app

# Copy app code
COPY . .

ENV PYTHONUNBUFFERED=1
 
EXPOSE 8000
 
CMD ["python", "-u", "app.py"]
 
